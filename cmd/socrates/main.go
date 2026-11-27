// Socrates API: self-hosted LLMs in one container. A web interface to
// install models from Hugging Face, one active model kept in memory by
// llama.cpp, and an OpenAI- and Anthropic-compatible API.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/saschazesiger/Z-Dash/server"
	"github.com/saschazesiger/Z-Dash/server/secret"

	"github.com/saschazesiger/SocratesApi/internal/api"
	"github.com/saschazesiger/SocratesApi/internal/convert"
	"github.com/saschazesiger/SocratesApi/internal/engine"
	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/proxy"
	"github.com/saschazesiger/SocratesApi/internal/store"
	"github.com/saschazesiger/SocratesApi/internal/sysinfo"
	"github.com/saschazesiger/SocratesApi/web"
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

	// The server keeps its state (the admin password, the sessions, the
	// tunnel) in Socrates' own database, under the keys Socrates used
	// before, and seals the tunnel's token with secret.key (or
	// SOCRATES_SECRET_KEY).
	sealer, err := secret.Load(dataDir, os.Getenv("SOCRATES_SECRET_KEY"))
	if err != nil {
		logger.Fatalf("secret key: %v", err)
	}
	// SOCRATES_LISTEN wins; else SOCRATES_HOST and SOCRATES_PORT, as before.
	listen := ""
	if os.Getenv("SOCRATES_LISTEN") == "" {
		listen = net.JoinHostPort(env("SOCRATES_HOST", ""), port)
	}
	// The API is built after the server; a sign-out that comes before (the
	// admin password set from the environment) finds no stream to end.
	var apiSrv atomic.Pointer[api.Server]
	app, err := server.New(server.Config{
		// The slug keeps the cookie (socrates_session) and the environment
		// prefix (SOCRATES_) of earlier versions.
		Name:    "Socrates API",
		Slug:    "socrates",
		Version: version,
		DataDir: dataDir,
		Listen:  listen,
		Assets:  web.FS,
		// Requests to the model API carry whole conversations and images,
		// up to 256 MB, from clients on slow links: no bound on how long
		// they take to arrive.
		ReadTimeout: -1,
		KV:          store.ServerKV{St: st},
		Sealer:      sealer,
		Logf:        logf,
		// The status stream and the chat are Socrates' own long-lived
		// requests: they end when their person is signed out.
		OnSignOut: func() {
			if s := apiSrv.Load(); s != nil {
				s.SignedOut()
			}
		},
	})
	if err != nil {
		logger.Fatalf("%v", err)
	}
	srv = api.New(api.Deps{
		App: app, Store: st, Engine: eng, Proxy: px, HF: hfc, Downloader: dl, Converter: conv,
		Version: version, ModelsDir: modelsDir, Logf: logf,
	})
	apiSrv.Store(srv)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engineDone := make(chan struct{})
	go func() { eng.Run(ctx); close(engineDone) }()
	go dl.Run(ctx)
	go conv.Run(ctx)
	go srv.Run(ctx)

	logf("Socrates API %s: open http://<this machine>:%s in a browser (the first visitor sets the admin password)", version, port)
	if err := app.Run(ctx); err != nil {
		logger.Fatalf("%v", err)
	}
	<-engineDone
}
