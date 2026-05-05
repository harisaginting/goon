package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/harisaginting/goon/internal/workflow"
)

// runWorkflow handles `goon workflow <action>`:
//
//	goon workflow                       alias for show
//	goon workflow show                  print resolved config (defaults+overrides)
//	goon workflow path                  print path goon will write to
//	goon workflow init                  write a starter workflow.json
//	goon workflow edit                  open the file in $EDITOR
//	goon workflow hooks                 list every supported hook name
func runWorkflow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	action := "show"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		rest = args[1:]
	}
	_ = rest

	switch action {
	case "show":
		return wfShow(stdout)
	case "path":
		fmt.Fprintln(stdout, workflow.DefaultConfigFilePath())
		return nil
	case "init":
		return wfInit(stdout)
	case "edit":
		return wfEdit(ctx, stdout, stderr)
	case "hooks":
		return wfListHooks(stdout)
	case "help", "-h", "--help":
		printWorkflowHelp(stdout)
		return nil
	default:
		printWorkflowHelp(stderr)
		return fmt.Errorf("unknown workflow action %q", action)
	}
}

func printWorkflowHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  goon workflow                  show the resolved workflow config")
	fmt.Fprintln(w, "  goon workflow show             same as above")
	fmt.Fprintln(w, "  goon workflow path             print path to the workflow.json file")
	fmt.Fprintln(w, "  goon workflow init             write a starter workflow.json (refuses to overwrite)")
	fmt.Fprintln(w, "  goon workflow edit             open workflow.json in $EDITOR")
	fmt.Fprintln(w, "  goon workflow hooks            list every supported hook name")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Resolution order (first match wins):")
	fmt.Fprintln(w, "  1. $GOON_WORKFLOW_FILE")
	fmt.Fprintln(w, "  2. ./.goon/workflow.json")
	fmt.Fprintln(w, "  3. $XDG_CONFIG_HOME/goon/workflow.json")
	fmt.Fprintln(w, "  4. ~/.config/goon/workflow.json")
	fmt.Fprintln(w, "  5. ~/.goon/workflow.json")
}

func wfShow(stdout io.Writer) error {
	cfg, source, err := workflow.LoadConfig("")
	if err != nil {
		return err
	}
	if source == "" {
		fmt.Fprintf(stdout, "(no workflow.json found — using built-in defaults)\n")
		fmt.Fprintf(stdout, "create one with:  goon workflow init\n\n")
	} else {
		fmt.Fprintf(stdout, "loaded from: %s\n\n", source)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_, _ = stdout.Write(data)
	fmt.Fprintln(stdout)
	return nil
}

func wfInit(stdout io.Writer) error {
	path := workflow.DefaultConfigFilePath()
	if err := workflow.SaveDefault(path); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote starter workflow config: %s\n", path)
	fmt.Fprintln(stdout, "edit it with:  goon workflow edit")
	return nil
}

func wfEdit(ctx context.Context, stdout, stderr io.Writer) error {
	path := workflow.DefaultConfigFilePath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Create from defaults so the editor opens something useful.
		if err := workflow.SaveDefault(path); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "(initialised empty config at %s)\n", path)
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func wfListHooks(stdout io.Writer) error {
	fmt.Fprintln(stdout, "Supported hook names (any of these can appear under \"hooks\":{}):")
	for _, h := range workflow.AllHooks {
		fmt.Fprintf(stdout, "  %s\n", h)
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Each hook is a JSON array of shell commands. They run via sh -c with these env vars set:")
	fmt.Fprintln(stdout, "  $TICKET_KEY    e.g. \"ENG-123\"")
	fmt.Fprintln(stdout, "  $TICKET_TITLE  e.g. \"Add login\"")
	fmt.Fprintln(stdout, "  $TICKET_URL    direct link to the ticket")
	fmt.Fprintln(stdout, "  $TICKET_SOURCE \"jira\" | \"github\"")
	fmt.Fprintln(stdout, "  $TICKET_PROJECT  Jira project key or \"owner/repo\"")
	fmt.Fprintln(stdout, "  $REPO          local repo path")
	fmt.Fprintln(stdout, "  $BRANCH        the branch goon will push (e.g. \"goon/eng-123\")")
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Or use Go template syntax inside the command: {{.Key}}, {{.Title}}, {{.Branch}}, …")
	return nil
}
