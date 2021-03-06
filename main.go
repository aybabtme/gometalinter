package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin"
)

type Severity string

// Linter message severity levels.
const (
	Warning Severity = "warning"
	Error   Severity = "error"
)

type Linter string

func (l Linter) Command() string {
	return string(l[0:strings.Index(string(l), ":")])
}

func (l Linter) Pattern() string {
	return string(l[strings.Index(string(l), ":"):])
}

var (
	predefinedPatterns = map[string]string{
		"PATH:LINE:COL:MESSAGE": `(?P<path>[^:]+):(?P<line>\d+):(?P<col>\d+):\s*(?P<message>.*)`,
		"PATH:LINE:MESSAGE":     `(?P<path>[^:]+):(?P<line>\d+):\s*(?P<message>.*)`,
	}
	lintersFlag = map[string]string{
		// main.go:8:10: should omit type map[string]string from declaration of var linters; it will be inferred from the right-hand side
		"golint": "golint {path}:PATH:LINE:COL:MESSAGE",
		// test/stutter.go:19: missing argument for Printf("%d"): format reads arg 1, have only 0 args
		"vet":         "go tool vet {path}:PATH:LINE:MESSAGE",
		"gotype":      "gotype {path}:PATH:LINE:COL:MESSAGE",
		"errcheck":    `errcheck {path}:(?P<path>[^:]+):(?P<line>\d+):(?P<col>\d+)\t(?P<message>.*)`,
		"varcheck":    "varcheck {path}:PATH:LINE:MESSAGE",
		"structcheck": "structcheck {path}:PATH:LINE:MESSAGE",
		"defercheck":  "defercheck {path}:PATH:LINE:MESSAGE",
	}
	linterMessageOverrideFlag = map[string]string{
		"errcheck":    "error return value not checked ({message})",
		"varcheck":    "unused global variable {message}",
		"structcheck": "unused struct field {message}",
	}
	linterSeverityFlag = map[string]string{
		"errcheck":    "warning",
		"golint":      "warning",
		"varcheck":    "warning",
		"structcheck": "warning",
	}
	installMap = map[string]string{
		"golint":      "go get github.com/golang/lint/golint",
		"gotype":      "go get code.google.com/p/go.tools/cmd/gotype",
		"errcheck":    "go get github.com/kisielk/errcheck",
		"defercheck":  "go get github.com/opennota/check/cmd/defercheck",
		"varcheck":    "go get github.com/opennota/check/cmd/varcheck",
		"structcheck": "go get github.com/opennota/check/cmd/structcheck",
	}
	pathArg            = kingpin.Arg("path", "Directory to lint.").Default(".").String()
	fastFlag           = kingpin.Flag("fast", "Only run fast linters.").Bool()
	installFlag        = kingpin.Flag("install", "Attempt to install all known linters.").Bool()
	disableLintersFlag = kingpin.Flag("disable", "List of linters to disable.").PlaceHolder("LINTER").Short('D').Strings()
	debugFlag          = kingpin.Flag("debug", "Display messages for failed linters, etc.").Short('d').Bool()
	concurrencyFlag    = kingpin.Flag("concurrency", "Number of concurrent linters to run.").Default("16").Short('j').Int()
	excludeFlag        = kingpin.Flag("exclude", "Exclude messages matching this regular expression.").PlaceHolder("REGEXP").String()
)

func init() {
	kingpin.Flag("linter", "Specify a linter.").PlaceHolder("NAME:COMMAND:PATTERN").StringMapVar(&lintersFlag)
	kingpin.Flag("message-overrides", "Override message from linter. {message} will be expanded to the original message.").PlaceHolder("LINTER:MESSAGE").StringMapVar(&linterMessageOverrideFlag)
	kingpin.Flag("severity", "Map of linter severities.").PlaceHolder("LINTER:SEVERITY").StringMapVar(&linterSeverityFlag)
}

type Issue struct {
	severity Severity
	path     string
	line     int
	col      int
	message  string
}

func (m *Issue) String() string {
	col := ""
	if m.col != 0 {
		col = fmt.Sprintf("%d", m.col)
	}
	return fmt.Sprintf("%s:%d:%s:%s: %s", m.path, m.line, col, m.severity, m.message)
}

type Issues []*Issue

func (m Issues) Len() int      { return len(m) }
func (m Issues) Swap(i, j int) { m[i], m[j] = m[j], m[i] }
func (m Issues) Less(i, j int) bool {
	return m[i].path < m[j].path || m[i].line < m[j].line || m[i].col < m[j].col
}

func debug(format string, args ...interface{}) {
	if *debugFlag {
		fmt.Fprintf(os.Stderr, "DEBUG: "+format+"\n", args...)
	}
}

func formatLinters() string {
	w := bytes.NewBuffer(nil)
	for command, description := range lintersFlag {
		linter := Linter(description)
		fmt.Fprintf(w, "    %s -> %s -> %s\n", command, linter.Command(), linter.Pattern())
	}
	return w.String()
}

func formatSeverity() string {
	w := bytes.NewBuffer(nil)
	for name, severity := range linterSeverityFlag {
		fmt.Fprintf(w, "    %s -> %s\n", name, severity)
	}
	return w.String()
}

func exArgs() (arg0 string, arg1 string) {
	if runtime.GOOS == "windows" {
		arg0 = "cmd"
		arg1 = "/C"
	} else {
		arg0 = "/bin/sh"
		arg1 = "-c"
	}
	return
}

func main() {
	kingpin.CommandLine.Help = fmt.Sprintf(`Aggregate and normalise the output of a whole bunch of Go linters.

Default linters:

%s

Severity override map (default is "error"):

%s
`, formatLinters(), formatSeverity())
	kingpin.Parse()
	var filter *regexp.Regexp
	if *excludeFlag != "" {
		filter = regexp.MustCompile(*excludeFlag)
	}

	if *fastFlag {
		*disableLintersFlag = append(*disableLintersFlag, "structcheck", "varcheck", "errcheck")
	}

	if *installFlag {
		for name, cmd := range installMap {
			fmt.Printf("Installing %s -> %s\n", name, cmd)
			arg0, arg1 := exArgs()
			c := exec.Command(arg0, arg1, cmd)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			err := c.Run()
			if err != nil {
				kingpin.CommandLine.Errorf(os.Stderr, "failed to install %s: %s", name, err)
			}
		}
		return
	}

	runtime.GOMAXPROCS(*concurrencyFlag)

	disable := map[string]bool{}
	for _, linter := range *disableLintersFlag {
		disable[linter] = true
	}

	start := time.Now()
	paths := *pathArg
	concurrency := make(chan bool, *concurrencyFlag)
	issues := make(chan *Issue, 100000)
	wg := &sync.WaitGroup{}
	for name, description := range lintersFlag {
		if _, ok := disable[name]; ok {
			debug("linter %s disabled", name)
			continue
		}
		parts := strings.SplitN(description, ":", 2)
		command := parts[0]
		pattern := parts[1]

		wg.Add(1)
		go func(name, command, pattern string) {
			concurrency <- true
			executeLinter(issues, name, command, pattern, paths)
			<-concurrency
			wg.Done()
		}(name, command, pattern)
	}

	wg.Wait()
	close(issues)
	for issue := range issues {
		if filter != nil && filter.MatchString(issue.String()) {
			continue
		}
		fmt.Printf("%s\n", issue)
	}
	elapsed := time.Now().Sub(start)
	debug("total elapsed time %s", elapsed)
}

func executeLinter(issues chan *Issue, name, command, pattern, paths string) {
	debug("linting with %s: %s", name, command)

	start := time.Now()
	if p, ok := predefinedPatterns[pattern]; ok {
		pattern = p
	}
	regexp.Compile(pattern)
	re, err := regexp.Compile(pattern)
	kingpin.FatalIfError(err, "invalid pattern for '"+command+"'")

	command = strings.Replace(command, "{path}", paths, -1)
	debug("executing %s", command)
	arg0, arg1 := exArgs()
	cmd := exec.Command(arg0, arg1, command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			debug("warning: %s failed: %s", command, err)
			return
		}
		debug("warning: %s returned %s", command, err)
	}

	for _, line := range bytes.Split(out, []byte("\n")) {
		groups := re.FindAllSubmatch(line, -1)
		if groups == nil {
			continue
		}
		issue := &Issue{}
		for i, name := range re.SubexpNames() {
			part := string(groups[0][i])
			switch name {
			case "path":
				issue.path = part

			case "line":
				n, err := strconv.ParseInt(part, 10, 32)
				kingpin.FatalIfError(err, "line matched invalid integer")
				issue.line = int(n)

			case "col":
				n, err := strconv.ParseInt(part, 10, 32)
				kingpin.FatalIfError(err, "col matched invalid integer")
				issue.col = int(n)

			case "message":
				issue.message = part

			case "":

			default:
				kingpin.Fatalf("invalid subgroup %s", name)
			}
		}
		if m, ok := linterMessageOverrideFlag[name]; ok {
			issue.message = strings.Replace(m, "{message}", issue.message, -1)
		}
		if sev, ok := linterSeverityFlag[name]; ok {
			issue.severity = Severity(sev)
		} else {
			issue.severity = "error"
		}
		issues <- issue
	}

	elapsed := time.Now().Sub(start)
	debug("%s linter took %s", name, elapsed)
}
