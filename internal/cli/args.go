package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type args struct {
	command string
	flags   map[string][]string
	pos     []string
}

func (a args) has(name string) bool { return len(a.flags[name]) > 0 }

func (a args) value(name string) string {
	values := a.flags[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func (a args) or(name, fallback string) string {
	if a.has(name) {
		return a.value(name)
	}
	return fallback
}

func (a args) enabled(name string) bool { return a.value(name) == "true" }

func (a args) pointer(name string) *string {
	if !a.has(name) {
		return nil
	}
	value := a.value(name)
	return &value
}

func (a args) intOption(name string, min int) (*int, error) {
	if !a.has(name) {
		return nil, nil
	}
	value, err := strconv.Atoi(a.value(name))
	if err != nil || value < min {
		return nil, fmt.Errorf("--%s 需要不小于 %d 的整数", name, min)
	}
	return &value, nil
}

func options(switches, values string) map[string]int {
	m := make(map[string]int)
	for _, name := range strings.Fields(switches) {
		m[name] = 0
	}
	for _, name := range strings.Fields(values) {
		m[name] = 1
	}
	return m
}

var globalOptions = options("json help version", "state-dir api-key")

var commandOptions = map[string]map[string]int{
	"auth":                  nil,
	"auth import":           options("force", "client token"),
	"auth set-api-key":      options("stdin", ""),
	"auth login":            options("", "authuser timeout"),
	"auth status":           nil,
	"auth logout":           options("all", ""),
	"whoami":                nil,
	"quota":                 options("reset-exhausted", "limit"),
	"sync":                  options("with-items", "playlist"),
	"list":                  options("remote", ""),
	"show":                  options("remote", ""),
	"create":                options("dry-run", "title description privacy"),
	"update":                options("dry-run", "title description privacy"),
	"delete":                options("yes dry-run", ""),
	"add":                   options("allow-duplicates dry-run", "file position"),
	"remove":                options("all-occurrences dry-run", "item-id video"),
	"dedupe":                options("dry-run", "keep"),
	"reorder":               {"move": 2, "by": 1, "dry-run": 0},
	"export":                options("all remote", "out format"),
	"migrate":               nil,
	"migrate init":          options("force", "plan record queue-file include exclude"),
	"migrate status":        options("", "queue-file"),
	"migrate run":           options("confirm-recreate confirm-reinsert dry-run", "queue-file budget max-writes playlist"),
	"migrate retry-failed":  options("include-permanent", "queue-file"),
	"migrate export-report": options("", "queue-file out format"),
}

func parse(argv []string) (a args, err error) {
	a.flags = make(map[string][]string)
	endOptions := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !endOptions && arg == "--" {
			endOptions = true
			continue
		}
		if !endOptions && (strings.HasPrefix(arg, "--") || arg == "-h" || arg == "-v") {
			if arg == "-h" {
				arg = "--help"
			}
			if arg == "-v" {
				arg = "--version"
			}
			name, inline, hasInline := strings.Cut(arg[2:], "=")
			count, known := globalOptions[name]
			if !known {
				count, known = commandOptions[a.command][name]
			}
			if !known {
				return a, errors.New("无法识别命令选项，请查看 --help")
			}
			if count == 0 {
				value := true
				if hasInline {
					value, err = strconv.ParseBool(inline)
					if err != nil {
						return a, fmt.Errorf("--%s 需要 true 或 false", name)
					}
				}
				a.flags[name] = []string{strconv.FormatBool(value)}
				continue
			}

			values := make([]string, 0, count)
			if hasInline {
				values = append(values, inline)
			}
			for len(values) < count {
				if i+1 == len(argv) || strings.HasPrefix(argv[i+1], "--") || argv[i+1] == "-h" || argv[i+1] == "-v" {
					return a, fmt.Errorf("--%s 需要 %d 个参数", name, count)
				}
				i++
				values = append(values, argv[i])
			}
			if name == "video" || name == "item-id" {
				a.flags[name] = append(a.flags[name], values...)
			} else {
				a.flags[name] = values
			}
			continue
		}
		switch a.command {
		case "":
			if _, known := commandOptions[arg]; !known || strings.ContainsRune(arg, ' ') {
				return a, errors.New("无法识别命令，请查看 ytpl --help")
			}
			a.command = arg
		case "auth", "migrate":
			full := a.command + " " + arg
			if _, known := commandOptions[full]; !known {
				return a, fmt.Errorf("无法识别子命令，请查看 ytpl %s --help", a.command)
			}
			a.command = full
		default:
			a.pos = append(a.pos, arg)
		}
	}
	return a, nil
}

// Use JSON for a syntax error even when the flag follows the offending token.
func jsonOnParseError(argv []string) bool {
	json := false
	for _, arg := range argv {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			json = true
		}
		if value, ok := strings.CutPrefix(arg, "--json="); ok {
			if b, err := strconv.ParseBool(value); err == nil {
				json = b
			}
		}
	}
	return json
}
