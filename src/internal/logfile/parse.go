package logfile

import (
	"strconv"
	"strings"
	"time"
)

// Field is one key=value of a line that isn't one of the fixed ones.
type Field struct{ Key, Value string }

// Entry is one parsed line: the fixed keys, and the other fields in the order they were written.
type Entry struct {
	Time      time.Time
	Level     string // DEBUG, INFO, WARN or ERROR
	Msg       string
	Component string
	PID       string
	Fields    []Field
}

// Get is the value of the field key, "" without one.
func (e Entry) Get(key string) string {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// Parse reads a line the writer wrote: key=value pairs, a value bare or in double quotes with Go escapes. A line that
// isn't that (an older format, a line cut short) is not an entry; ok is false.
func Parse(line string) (e Entry, ok bool) {
	rest := strings.TrimRight(line, "\r\n")
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 || strings.ContainsAny(rest[:eq], " \"") {
			return Entry{}, false
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := -1
			for i := 1; i < len(rest); i++ {
				if rest[i] == '\\' {
					i++
					continue
				}
				if rest[i] == '"' {
					end = i
					break
				}
			}
			if end < 0 {
				return Entry{}, false
			}
			v, err := strconv.Unquote(rest[:end+1])
			if err != nil {
				return Entry{}, false
			}
			val, rest = v, rest[end+1:]
		} else if sp := strings.IndexByte(rest, ' '); sp < 0 {
			val, rest = rest, ""
		} else {
			val, rest = rest[:sp], rest[sp+1:]
		}
		switch key {
		case "time":
			t, err := time.Parse(time.RFC3339, val)
			if err != nil {
				return Entry{}, false
			}
			e.Time = t
		case "level":
			e.Level = val
		case "msg":
			e.Msg = val
		case "component":
			e.Component = val
		case "pid":
			e.PID = val
		default:
			e.Fields = append(e.Fields, Field{key, val})
		}
	}
	return e, e.Level != "" && !e.Time.IsZero()
}
