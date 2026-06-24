package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type entry struct {
	Name  string
	Path  string
	IsDir bool
	Size  int64
	Ready bool
	Err   error
}

type cachedSize struct {
	Size  int64
	Ready bool
	Err   error
}

type app struct {
	mu       sync.Mutex
	cwd      string
	entries  []entry
	selected int
	offset   int
	cache    map[string]cachedSize
	jobs     map[string]bool
	updates  chan string
	done     chan struct{}
	version  int
}

type termState struct {
	state syscall.Termios
}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func main() {
	start := "."
	if len(os.Args) > 1 {
		start = os.Args[1]
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	info, err := os.Stat(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	oldState, err := enableRawMode()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer restoreTerminal(oldState)

	fmt.Print("\x1b[?25l\x1b[2J\x1b[H")
	defer fmt.Print("\x1b[?25h\x1b[0m\x1b[2J\x1b[H")

	a := &app{
		cwd:     abs,
		cache:   make(map[string]cachedSize),
		jobs:    make(map[string]bool),
		updates: make(chan string, 64),
		done:    make(chan struct{}),
	}
	a.loadDir(abs)

	keys := make(chan string, 32)
	go readKeys(keys, a.done)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	a.render()
	for {
		select {
		case key := <-keys:
			if !a.handleKey(key) {
				close(a.done)
				return
			}
			a.render()
		case <-a.updates:
			a.refreshCachedEntries()
			a.render()
		case <-ticker.C:
			a.render()
		}
	}
}

func (a *app) handleKey(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch key {
	case "q", "ctrl-c":
		return false
	case "up":
		if a.selected > 0 {
			a.selected--
		}
	case "down":
		if a.selected < len(a.entries)-1 {
			a.selected++
		}
	case "right", "enter":
		if len(a.entries) > 0 && a.entries[a.selected].IsDir {
			path := a.entries[a.selected].Path
			a.mu.Unlock()
			a.loadDir(path)
			a.mu.Lock()
		}
	case "left":
		parent := filepath.Dir(a.cwd)
		if parent != a.cwd {
			a.mu.Unlock()
			a.loadDir(parent)
			a.mu.Lock()
		}
	case "f5":
		a.clearSubCacheLocked(a.cwd)
		cwd := a.cwd
		a.mu.Unlock()
		a.loadDir(cwd)
		a.mu.Lock()
	}
	return true
}

func (a *app) loadDir(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}

	dirs, readErr := os.ReadDir(abs)
	entries := make([]entry, 0, len(dirs))
	for _, d := range dirs {
		p := filepath.Join(abs, d.Name())
		isDir := d.IsDir()
		e := entry{Name: d.Name(), Path: p, IsDir: isDir}
		if isDir {
			if c, ok := a.cache[p]; ok && c.Ready {
				e.Size = c.Size
				e.Ready = true
				e.Err = c.Err
			}
		} else {
			if info, err := d.Info(); err == nil {
				e.Size = info.Size()
				e.Ready = true
			} else {
				e.Ready = true
				e.Err = err
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	a.mu.Lock()
	a.cwd = abs
	a.entries = entries
	a.selected = 0
	a.offset = 0
	a.version++
	version := a.version
	a.mu.Unlock()

	if readErr == nil {
		for _, e := range entries {
			if e.IsDir && !e.Ready {
				a.startSizeJob(e.Path, version)
			}
		}
	}
}

func (a *app) startSizeJob(path string, version int) {
	a.mu.Lock()
	if a.jobs[path] {
		a.mu.Unlock()
		return
	}
	a.jobs[path] = true
	a.mu.Unlock()

	go func() {
		size, err := dirSize(path)
		a.mu.Lock()
		a.cache[path] = cachedSize{Size: size, Ready: true, Err: err}
		delete(a.jobs, path)
		current := a.version == version
		for i := range a.entries {
			if a.entries[i].Path == path {
				a.entries[i].Size = size
				a.entries[i].Ready = true
				a.entries[i].Err = err
				break
			}
		}
		a.mu.Unlock()
		if current {
			select {
			case a.updates <- path:
			default:
			}
		}
	}()
}

func (a *app) refreshCachedEntries() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.entries {
		if a.entries[i].IsDir {
			if c, ok := a.cache[a.entries[i].Path]; ok && c.Ready {
				a.entries[i].Size = c.Size
				a.entries[i].Ready = true
				a.entries[i].Err = c.Err
			}
		}
	}
}

func (a *app) clearSubCacheLocked(root string) {
	root = filepath.Clean(root)
	prefix := root + string(os.PathSeparator)
	for path := range a.cache {
		clean := filepath.Clean(path)
		if clean == root || strings.HasPrefix(clean, prefix) {
			delete(a.cache, path)
		}
	}
}

func dirSize(root string) (int64, error) {
	var total int64
	var firstErr error
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return total, firstErr
}

func (a *app) render() {
	a.mu.Lock()
	defer a.mu.Unlock()

	rows, cols := terminalSize()
	if rows < 5 {
		rows = 5
	}
	if cols < 40 {
		cols = 40
	}

	listRows := rows - 3
	if a.selected < a.offset {
		a.offset = a.selected
	}
	if a.selected >= a.offset+listRows {
		a.offset = a.selected - listRows + 1
	}

	total := int64(0)
	for _, e := range a.entries {
		if e.Ready {
			total += e.Size
		}
	}

	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	fmt.Fprintf(&b, "ncdu-go  %s\r\n", truncate(a.cwd, cols-10))
	fmt.Fprintf(&b, "↑/↓ move  → enter  ← parent  F5 refresh  q quit  total: %s\r\n", formatSize(total))

	for row := 0; row < listRows; row++ {
		idx := a.offset + row
		if idx >= len(a.entries) {
			b.WriteString("~\r\n")
			continue
		}
		e := a.entries[idx]
		selected := idx == a.selected
		if selected {
			b.WriteString("\x1b[7m")
		}
		line := formatEntry(e, total, cols)
		b.WriteString(line)
		if selected {
			b.WriteString("\x1b[0m")
		}
		b.WriteString("\r\n")
	}

	fmt.Print(b.String())
}

func formatEntry(e entry, total int64, cols int) string {
	name := e.Name
	if e.IsDir {
		name += "/"
	}
	if e.Err != nil {
		name += " !"
	}

	sizeText := "scanning..."
	if e.Ready {
		sizeText = formatSize(e.Size)
	}

	barWidth := 22
	nameWidth := cols - barWidth - len(sizeText) - 8
	if nameWidth < 10 {
		nameWidth = 10
	}
	if nameWidth > 60 {
		nameWidth = 60
	}

	percent := 0.0
	if total > 0 && e.Ready {
		percent = float64(e.Size) / float64(total)
	}
	bar := progressBar(percent, barWidth)

	line := fmt.Sprintf("%-*s  %s  %10s", nameWidth, truncate(name, nameWidth), bar, sizeText)
	return truncate(line, cols)
}

func progressBar(percent float64, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	filled := int(percent * float64(width))
	if percent > 0 && filled == 0 {
		filled = 1
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func formatSize(size int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(size)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%dB", size)
	}
	return fmt.Sprintf("%.2f%s", value, units[unit])
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

func readKeys(keys chan<- string, done <-chan struct{}) {
	buf := make([]byte, 16)
	for {
		select {
		case <-done:
			return
		default:
		}

		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			continue
		}
		seq := string(buf[:n])
		switch {
		case seq == "\x03":
			keys <- "ctrl-c"
		case seq == "q":
			keys <- "q"
		case seq == "\r" || seq == "\n":
			keys <- "enter"
		case strings.Contains(seq, "\x1b[A"):
			keys <- "up"
		case strings.Contains(seq, "\x1b[B"):
			keys <- "down"
		case strings.Contains(seq, "\x1b[C"):
			keys <- "right"
		case strings.Contains(seq, "\x1b[D"):
			keys <- "left"
		case strings.Contains(seq, "\x1b[15~"):
			keys <- "f5"
		}
	}
}

func enableRawMode() (*termState, error) {
	fd := os.Stdin.Fd()
	var old syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&old)))
	if errno != 0 {
		return nil, errno
	}
	newState := old
	newState.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	newState.Oflag &^= syscall.OPOST
	newState.Cflag |= syscall.CS8
	newState.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	newState.Cc[syscall.VMIN] = 1
	newState.Cc[syscall.VTIME] = 0
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&newState)))
	if errno != 0 {
		return nil, errno
	}
	return &termState{state: old}, nil
}

func restoreTerminal(old *termState) {
	if old == nil {
		return
	}
	fd := os.Stdin.Fd()
	syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&old.state)))
}

func terminalSize() (int, int) {
	ws := &winsize{}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(ws)))
	if errno != 0 || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}
