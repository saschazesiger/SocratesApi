package engine

import (
	"os"
	"syscall"
	"unsafe"
)

// resident counts how many bytes of the given ranges of the model files are
// in the page cache right now (mincore). For weights that llama.cpp maps
// from the file, that is exactly "in RAM"; the rest is read from the NVMe
// when it is needed.
func resident(paths []string, ranges []fileRange) int64 {
	page := int64(os.Getpagesize())
	var total int64
	byFile := map[int][]fileRange{}
	for _, r := range ranges {
		byFile[r.file] = append(byFile[r.file], r)
	}
	for idx, rs := range byFile {
		if idx >= len(paths) {
			continue
		}
		f, err := os.Open(paths[idx])
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil || st.Size() == 0 {
			f.Close()
			continue
		}
		data, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		f.Close()
		if err != nil {
			continue
		}
		vec := make([]byte, 1<<16) // 256 MiB of file per call with 4 KiB pages
		for _, r := range rs {
			start := r.start / page * page
			end := r.end
			if end > st.Size() {
				end = st.Size()
			}
			for off := start; off < end; {
				n := int64(len(vec)) * page
				if off+n > end {
					n = end - off
				}
				pages := (n + page - 1) / page
				_, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&data[off])), uintptr(n), uintptr(unsafe.Pointer(&vec[0])))
				if e != 0 {
					break
				}
				for i := int64(0); i < pages; i++ {
					if vec[i]&1 != 0 {
						total += page
					}
				}
				off += n
			}
		}
		_ = syscall.Munmap(data)
	}
	return total
}

// preload reads the ranges once, so the weights that live in RAM are in
// the page cache before the first request instead of during it. It stops
// when stop is closed and reads at most limit bytes.
func preload(paths []string, ranges []fileRange, limit int64, stop <-chan struct{}) {
	buf := make([]byte, 8<<20)
	var done int64
	for _, r := range ranges {
		if r.file >= len(paths) {
			continue
		}
		f, err := os.Open(paths[r.file])
		if err != nil {
			continue
		}
		for off := r.start; off < r.end; {
			select {
			case <-stop:
				f.Close()
				return
			default:
			}
			n := int64(len(buf))
			if off+n > r.end {
				n = r.end - off
			}
			if _, err := f.ReadAt(buf[:n], off); err != nil {
				break
			}
			off += n
			done += n
			if done >= limit {
				f.Close()
				return
			}
		}
		f.Close()
	}
}
