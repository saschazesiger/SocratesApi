// Socrates API: self-hosted LLMs in one container. A web interface to
// install models from Hugging Face, one active model kept in memory by
// llama.cpp, and an OpenAI- and Anthropic-compatible API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/api"
	"github.com/saschazesiger/SocratesApi/internal/convert"
	"github.com/saschazesiger/SocratesApi/internal/engine"
	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/proxy"
	"github.com/saschazesiger/SocratesApi/internal/store"
	"github.com/saschazesiger/SocratesApi/internal/sysinfo"
)

var version = "dev"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	logf := logger.Printf
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "version":
			fmt.Println(version)
			return
		case "healthcheck":
			// For Docker's HEALTHCHECK: the web server answers.
			c := &http.Client{Timeout: 4 * time.Second}
			resp, err := c.Get("http://127.0.0.1:" + env("SOCRATES_PORT", "8080") + "/healthz")
			if err != nil || resp.StatusCode != http.StatusOK {
				os.Exit(1)
			}
			return
		}
	}

	dataDir := env("SOCRATES_DATA", "/data")
	port := env("SOCRATES_PORT", "8080")
	modelsDir := filepath.Join(dataDir, "models")
	for _, d := range []string{dataDir, modelsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			logger.Fatalf("data directory %s: %v", d, err)
		}
	}
	st, err := store.Open(dataDir)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer st.Close()

	enginePort, _ := strconv.Atoi(env("SOCRATES_ENGINE_PORT", "8181"))
	sys := &sysinfo.Reader{DataDir: dataDir}
	eng := engine.New(engine.Config{
		ServerBin: env("SOCRATES_LLAMA_SERVER", "/app/llama-server"),
		LlamaBin:  env("SOCRATES_LLAMA", "/app/llama"),
		Port:      enginePort,
		ModelsDir: modelsDir,
	}, st, sys)
	eng.Logf = logf

	var srv *api.Server
	hfc := hf.NewClient(func() string {
		if srv == nil {
			return ""
		}
		return srv.HFToken()
	})
	dl := hf.NewDownloader(hfc)
	conv := convert.New(convert.Config{
		Python:     env("SOCRATES_PYTHON", "python3"),
		ScriptsDir: env("SOCRATES_CONVERT_SCRIPTS", "/opt/llama-convert"),
		ToolsDir:   filepath.Join(dataDir, "tools"),
		LlamaBin:   env("SOCRATES_LLAMA", "/app/llama"),
		ModelsDir:  modelsDir,
	}, st, hfc)
	px := proxy.New(eng, st)
	px.Logf = logf
	if s := os.Getenv("SOCRATES_LOAD_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			px.LoadTimeout = d
		}
	}

	var hosts []string
	for _, h := range strings.Split(os.Getenv("SOCRATES_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	srv = api.New(api.Deps{
		Store: st, Engine: eng, Proxy: px, HF: hfc, Downloader: dl, Converter: conv,
		Version: version, ModelsDir: modelsDir, ResetPassword: os.Getenv("SOCRATES_ADMIN_PASSWORD"),
		Hosts: hosts, Logf: logf,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engineDone := make(chan struct{})
	go func() { eng.Run(ctx); close(engineDone) }()
	go dl.Run(ctx)
	go conv.Run(ctx)
	go srv.Run(ctx)

	httpSrv := &http.Server{
		Addr:              net.JoinHostPort(env("SOCRATES_HOST", ""), port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()
	logf("Socrates API %s: open http://<this machine>:%s in a browser (the first visitor sets the admin password)", version, port)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("listen on %s: %v", httpSrv.Addr, err)
	}
	<-engineDone
}
