package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	fmt.Println("⚡ [Trade Engine Launcher]")
	fmt.Println("Stopping any running Trade Engine instances...")

	myPid := os.Getpid()

	// 1. Terminate existing trade-engine processes (except this launcher)
	_ = exec.Command("taskkill", "/F", "/FI", fmt.Sprintf("PID ne %d", myPid), "/IM", "trade-engine*").Run()

	// 2. Free port 8080 if occupied by another process
	psCmd := "Get-NetTCPConnection -LocalPort 8080 -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique"
	out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if pid, err := strconv.Atoi(line); err == nil && pid > 0 && pid != myPid {
				fmt.Printf("Freeing port 8080 (terminating PID %d)...\n", pid)
				_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
			}
		}
	}

	time.Sleep(500 * time.Millisecond)

	ex, err := os.Executable()
	dir := "."
	if err == nil {
		dir = filepath.Dir(ex)
	}
	targetExe := filepath.Join(dir, "trade-engine.exe")

	fmt.Println("🚀 Launching Trade Engine on http://localhost:8080 ...")
	cmd := exec.Command(targetExe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Dir = dir

	if err := cmd.Run(); err != nil {
		fmt.Printf("Process exited: %v\n", err)
	}
}
