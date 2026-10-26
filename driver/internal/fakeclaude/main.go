// Command fakeclaude stands in for the claude binary in the driver's
// tests: it runs in the same sandbox and prints stream-json events as
// claude -p does. The first word of the prompt (the argument after "--")
// picks what it does; see the cases in main. It is a test fixture, never
// shipped.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const version = "0.0.0-fake"

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(version + " (Claude Code)")
		return
	}
	if len(args) == 2 && args[0] == "sleepchild" {
		time.Sleep(time.Hour)
		return
	}
	prompt := ""
	if i := slices.Index(args, "--"); i >= 0 && i+1 < len(args) {
		prompt = args[i+1]
	}
	mode, rest, _ := strings.Cut(prompt, " ")

	switch mode {
	case "badinit":
		emitInit(args, []string{"Bash", "Edit", "Read"})
		time.Sleep(time.Hour)
		return
	case "noinit":
		emit(map[string]any{"type": "assistant"})
		result("success", false, "no init", model(args))
		return
	case "slowinit":
		time.Sleep(time.Hour)
		return
	}
	emitInit(args, toolsArg(args))
	emit(map[string]any{"type": "assistant", "message": "working"})

	switch mode {
	case "edit":
		f, err := os.OpenFile("tunables.go", os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			fail(err)
		}
		f.WriteString("\n// fake edit\n")
		f.Close()
		result("success", false, "  Shortened acceptWait.\n", model(args))
	case "noop":
		result("success", false, "Changed nothing.", model(args))
	case "report":
		env := os.Environ()
		slices.Sort(env)
		wd, _ := os.Getwd()
		b, _ := json.Marshal(map[string]any{"argv": args, "env": env, "cwd": wd})
		result("success", false, string(b), model(args))
	case "probe":
		b, _ := json.Marshal(probe())
		result("success", false, string(b), model(args))
	case "hang":
		// A child that outlives nothing: the test checks it is gone once
		// Mutate returns. rest carries the test's nonce into its argv.
		exec.Command("/proc/self/exe", "sleepchild", rest).Start()
		time.Sleep(time.Hour)
	case "symlink":
		os.Remove("tunables.go")
		os.Symlink("/etc/hosts", "tunables.go")
		result("success", false, "Swapped a file.", model(args))
	case "extra":
		os.WriteFile("extra.go", []byte("package wrapper\n"), 0o600)
		result("success", false, "Added a file.", model(args))
	case "maxturns":
		result("error_max_turns", true, "", model(args))
	case "wrongmodel":
		result("success", false, "Ran elsewhere.", "claude-haiku-5-5")
	case "flood":
		line := strings.Repeat("x", 1000)
		for range 10000 {
			emit(map[string]any{"type": "assistant", "message": line})
		}
		result("success", false, "Flooded.", model(args))
	case "exit1":
		result("success", false, "Then failed.", model(args))
		os.Exit(1)
	case "grow":
		f, err := os.OpenFile("tunables.go", os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			fail(err)
		}
		f.Write(make([]byte, 1<<20))
		f.Close()
		result("success", false, "Grew a file.", model(args))
	case "resulthang":
		result("success", false, "Done, then stuck.", model(args))
		time.Sleep(time.Hour)
	case "noresult":
	default:
		fail(fmt.Errorf("unknown mode %q", mode))
	}
}

func emit(v any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fakeclaude:", err)
	os.Exit(2)
}

// flag returns the value after name in args.
func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func model(args []string) string { return flag(args, "--model") }

// toolsArg is the tool set --tools asks for, as the real init event
// reports it.
func toolsArg(args []string) []string {
	var tools []string
	for t := range strings.SplitSeq(flag(args, "--tools"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			tools = append(tools, t)
		}
	}
	return tools
}

// emitInit prints the init event from what the arguments asked for, so
// the driver's check of it also checks the arguments.
func emitInit(args, tools []string) {
	wd, _ := os.Getwd()
	emit(map[string]any{
		"type": "system", "subtype": "init", "cwd": wd, "tools": tools,
		"mcp_servers": []any{}, "model": model(args),
		"permissionMode": flag(args, "--permission-mode"), "claude_code_version": version,
	})
}

func result(subtype string, isError bool, text, model string) {
	emit(map[string]any{
		"type": "result", "subtype": subtype, "is_error": isError, "result": text,
		"session_id": "fake-session", "num_turns": 3, "duration_ms": 1234, "total_cost_usd": 0.01,
		"usage": map[string]any{
			"input_tokens": 100, "output_tokens": 20,
			"cache_read_input_tokens": 5, "cache_creation_input_tokens": 7,
		},
		"modelUsage": map[string]any{model: map[string]any{}},
	})
}

// probe walks the whole tree the sandbox shows, but /proc, /dev and /sys,
// and reports its top-level entries and every path that could hold the
// fixed core or the repo: a path through this module, hashcore, the test
// vectors, alexedwards/argon2id or golang.org/x/crypto (whose argon2 the
// core uses); a .go file declaring package hashcore; the repo's go.mod;
// any .git. A host's other Argon2 code (libargon2.so, a Python argon2.py
// under /usr/lib) is not the fixed core, and the session cannot read
// /usr/lib anyway (--restricted confines Read to the work directory).
func probe() map[string]any {
	var top, found []string
	ents, _ := os.ReadDir("/")
	for _, e := range ents {
		top = append(top, e.Name())
	}
	n := 0
	filepath.WalkDir("/", func(p string, e fs.DirEntry, err error) error {
		if p == "/proc" || p == "/dev" || p == "/sys" {
			return filepath.SkipDir
		}
		n++
		lower := strings.ToLower(p)
		hit := filepath.Base(p) == ".git"
		for _, s := range []string{"codeinthecreek", "hashcore", "testvectors", "alexedwards", "golang.org/x/crypto"} {
			hit = hit || strings.Contains(lower, s)
		}
		if e != nil && e.Type().IsRegular() && (strings.HasSuffix(p, ".go") || filepath.Base(p) == "go.mod") {
			b, _ := os.ReadFile(p)
			hit = hit || strings.Contains(string(b), "package hashcore") || strings.Contains(string(b), "module github.com/codeinthecreek")
		}
		if hit {
			found = append(found, p)
		}
		return nil
	})
	return map[string]any{"top": top, "found": found, "walked": n}
}
