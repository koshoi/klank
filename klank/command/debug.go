package command

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"klank/cli"
)

// Debug implements `klank debug`: internal database inspection tools.
func Debug(db *sql.DB, args []string) {
	if len(args) == 0 {
		cli.Fatal("usage: klank debug <query|data> [args]\n  query <sql>  run a read-only SQL query\n  data         show tables and their schemas")
	}
	switch args[0] {
	case "query":
		if len(args) != 2 {
			cli.Fatal("usage: klank debug query <sql>")
		}
		debugQuery(db, args[1])
	case "data":
		debugData(db)
	default:
		cli.Fatal("unknown debug subcommand: %s", args[0])
	}
}

func debugQuery(db *sql.DB, query string) {
	// allow only read-only statements
	stmt := strings.TrimSpace(strings.ToLower(query))
	for _, prefix := range []string{"select", "with", "pragma", "explain"} {
		if strings.HasPrefix(stmt, prefix) {
			goto ok
		}
	}
	cli.Fatal("only read-only queries are allowed (select/with/pragma/explain)")
ok:
	rows, err := db.Query(query)
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	defer rows.Close()
	res, err := cli.ScanRows(rows)
	if err != nil {
		cli.Fatal("scan: %v", err)
	}
	cli.PrintResult(res)
}

func debugData(db *sql.DB) {
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	defer rows.Close()
	res, err := cli.ScanRows(rows)
	if err != nil {
		cli.Fatal("scan: %v", err)
	}
	if cli.IsJSON() {
		debugDataJSON(db, res)
		return
	}
	for _, row := range res.Rows {
		name := fmt.Sprintf("%v", row[0])
		schema := fmt.Sprintf("%v", row[1])
		fmt.Printf("table: %s\n%s\n\n", name, schema)
	}
}

// jsonColumn describes one column from PRAGMA table_info.
type jsonColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  any    `json:"default"`
}

// jsonTable describes one table from sqlite_master + table_info.
type jsonTable struct {
	Name       string       `json:"name"`
	Columns    []jsonColumn `json:"columns"`
	ColumnList []string     `json:"column_list"`
	PrimaryKey []string     `json:"primary_key"`
}

// debugDataJSON prints structured schema info: per table, the parsed columns
// (name, type, constraints) from PRAGMA table_info, plus the raw CREATE sql.
func debugDataJSON(db *sql.DB, res *cli.QueryResult) {
	tables := make([]jsonTable, 0, len(res.Rows))
	for _, row := range res.Rows {
		t := jsonTable{
			Name: fmt.Sprintf("%v", row[0]),
		}
		cols, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, t.Name))
		if err != nil {
			cli.Fatal("pragma table_info(%s): %v", t.Name, err)
		}
		colRes, err := cli.ScanRows(cols)
		cols.Close()
		if err != nil {
			cli.Fatal("scan: %v", err)
		}
		t.Columns = []jsonColumn{}
		t.ColumnList = []string{}
		t.PrimaryKey = []string{}
		for _, c := range colRes.Rows {
			// table_info columns: cid, name, type, notnull, dflt_value, pk
			name := fmt.Sprintf("%v", c[1])
			isPK := fmt.Sprintf("%v", c[5]) != "0"
			// SQLite quirk: PRAGMA table_info reports notnull=0 for INTEGER
			// PRIMARY KEY columns even though they can never be NULL.
			notNull := fmt.Sprintf("%v", c[3]) == "1" || isPK
			col := jsonColumn{
				Name:     name,
				Type:     strings.ToUpper(fmt.Sprintf("%v", c[2])),
				Nullable: !notNull,
				Default:  c[4], // nil when no default
			}
			t.Columns = append(t.Columns, col)
			t.ColumnList = append(t.ColumnList, col.Name)
			if isPK {
				t.PrimaryKey = append(t.PrimaryKey, name)
			}
		}
		tables = append(tables, t)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(tables); err != nil {
		cli.Fatal("encode json: %v", err)
	}
}
