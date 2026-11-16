// Command fakeagent stands in for a real CLI agent in integration tests.
//
// On start it appends one JSON line to $NOSTROMO_HOME/fakeagent.log with its
// arguments, working directory, pid and NOSTROMO_* environment. Then it reads
// lines from stdin:
//
//	exit N  exit with code N
//	size    print the terminal size as "size:<rows> <cols>"
//	other   print "echo:<line>"
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	logStart()
	fmt.Print("fakeagent ready> ")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "exit"):
			code, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "exit")))
			fmt.Printf("bye %d\n", code)
			os.Exit(code)
		case line == "size":
			cmd := exec.Command("stty", "size")
			cmd.Stdin = os.Stdin
			out, err := cmd.Output()
			if err != nil {
				fmt.Printf("size:error %v\n", err)
			} else {
				fmt.Printf("size:%s\n", strings.TrimSpace(string(out)))
			}
		default:
			fmt.Printf("echo:%s\n", line)
		}
		fmt.Print("fakeagent ready> ")
	}
}

func logStart() {
	home := os.Getenv("NOSTROMO_HOME")
	if home == "" {
		return
	}
	cwd, _ := os.Getwd()
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "NOSTROMO_") {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
	}
	entry, _ := json.Marshal(map[string]any{
		"args": os.Args[1:],
		"cwd":  cwd,
		"pid":  os.Getpid(),
		"env":  env,
	})
	f, err := os.OpenFile(filepath.Join(home, "fakeagent.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(entry, '\n'))
}
