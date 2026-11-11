// Package cli holds the small cross-cutting helpers shared by all klank
// packages: debug/error output, the selected output format, and generic
// result printing (text or json).
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// DebugMode enables [debug] output when set (klank -d).
var DebugMode bool

// OutputFormat controls how commands print results ("text" or "json").
var OutputFormat = "text"

// IsJSON reports whether the output format is json.
func IsJSON() bool { return OutputFormat == "json" }

// RawJSON is a string holding raw JSON (e.g. a stored column value) that
// should be embedded verbatim into json output instead of being quoted as
// a string. In text mode it prints like any other string. An empty value
// encodes as null.
type RawJSON string

// MarshalJSON embeds the raw bytes verbatim (null when empty).
func (r RawJSON) MarshalJSON() ([]byte, error) {
	if r == "" {
		return []byte("null"), nil
	}
	return []byte(r), nil
}

// Debug prints a [debug] line when DebugMode is enabled.
func Debug(format string, a ...any) {
	if DebugMode {
		fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", a...)
	}
}

// Fatal prints an [error] line and exits with status 1.
func Fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[error] "+format+"\n", a...)
	os.Exit(1)
}

// Warn prints a [warn] line to stderr without exiting.
func Warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[warn] "+format+"\n", a...)
}

// MustCwd returns the current working directory or exits.
func MustCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		Fatal("getwd: %v", err)
	}
	return cwd
}

// ---- generic query result printing (text/json) ----

// QueryResult is a generic result set: column names plus raw row values.
type QueryResult struct {
	Columns []string
	Rows    [][]any
}

// ScanRows drains a *sql.Rows into a QueryResult with generic column values.
func ScanRows(rows RowsScanner) (*QueryResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Columns: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// RowsScanner is the subset of *sql.Rows used by ScanRows.
type RowsScanner interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// PrintResult prints a QueryResult as json or as tab-separated text.
func PrintResult(res *QueryResult) {
	if IsJSON() {
		// rows as array of objects
		objs := make([]map[string]any, 0, len(res.Rows))
		for _, row := range res.Rows {
			obj := make(map[string]any, len(res.Columns))
			for i, c := range res.Columns {
				obj[c] = row[i]
			}
			objs = append(objs, obj)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(objs); err != nil {
			Fatal("encode json: %v", err)
		}
		return
	}
	for _, row := range res.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprintf("%v", v)
		}
		fmt.Println(strings.Join(parts, "\t"))
	}
}
