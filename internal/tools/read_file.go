package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// ReadFile reads up to 64KB from a file.
type ReadFile struct{}

func (*ReadFile) Name() string        { return "read_file" }
func (*ReadFile) Description() string { return "read up to 64KB of a file as UTF-8 text" }
func (*ReadFile) Schema() map[string]string {
	return map[string]string{"path": "filesystem path"}
}

func (*ReadFile) Run(_ context.Context, args map[string]string) (Result, error) {
	path := args["path"]
	if path == "" {
		return Result{ToolName: "read_file"}, errors.New(`read_file: "path" is required`)
	}
	f, err := os.Open(path)
	if err != nil {
		return Result{ToolName: "read_file", Err: err}, err
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return Result{ToolName: "read_file", Err: err}, err
	}
	return Result{ToolName: "read_file", Stdout: string(buf[:n])}, nil
}

// ListDir lists names in a directory (max 100, sorted).
type ListDir struct{}

func (*ListDir) Name() string        { return "list_dir" }
func (*ListDir) Description() string { return "list up to 100 entries in a directory" }
func (*ListDir) Schema() map[string]string {
	return map[string]string{"path": "directory path"}
}

func (*ListDir) Run(_ context.Context, args map[string]string) (Result, error) {
	path := args["path"]
	if path == "" {
		path = "."
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return Result{ToolName: "list_dir", Err: err}, err
	}
	limit := 100
	if len(entries) < limit {
		limit = len(entries)
	}
	out := ""
	for i := 0; i < limit; i++ {
		e := entries[i]
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		out += fmt.Sprintf("%s%s\n", e.Name(), suffix)
	}
	if len(entries) > limit {
		out += fmt.Sprintf("...(+%d more)\n", len(entries)-limit)
	}
	return Result{ToolName: "list_dir", Stdout: out}, nil
}

// Finish ends the agent loop with a summary message.
type Finish struct{}

func (*Finish) Name() string        { return "finish" }
func (*Finish) Description() string { return "end the task with a summary message" }
func (*Finish) Schema() map[string]string {
	return map[string]string{"message": "final user-facing summary"}
}

func (*Finish) Run(_ context.Context, args map[string]string) (Result, error) {
	msg := args["message"]
	if msg == "" {
		msg = "done"
	}
	return Result{ToolName: "finish", Stdout: msg}, nil
}
