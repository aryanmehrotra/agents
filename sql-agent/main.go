// sql-agent — a GoFr 1.58 service that turns a natural-language question into a real SQL query,
// runs it against its own datasource, and answers grounded in the actual result rows. Text-to-SQL
// over a connected warehouse/database is one of the most common "BI agent" deployments going into
// production right now (natural-language querying, dashboard generation, ad-hoc data exploration).
//
// The datasource is zero-config: GoFr wires up `c.SQL` straight from DB_DIALECT/DB_NAME in the env
// (no AddSQL call needed), so this ships against a local SQLite file seeded with a small sales
// dataset at startup — swap DB_DIALECT for mysql/postgres to point it at a real warehouse.
//
// Because the model's output is executed directly, the generated SQL is put through a guardrail
// before it ever reaches the database: only a single, read-only SELECT is allowed through.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

// schemaSQL is both the DDL executed at startup and the schema the LLM is shown — one source of
// truth, so the model never generates SQL against a schema that doesn't match what's actually there.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS departments (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS employees (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    department_id INTEGER NOT NULL REFERENCES departments(id),
    role          TEXT NOT NULL,
    hire_date     TEXT NOT NULL,
    salary        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS deals (
    id          INTEGER PRIMARY KEY,
    employee_id INTEGER NOT NULL REFERENCES employees(id),
    customer    TEXT NOT NULL,
    amount      INTEGER NOT NULL,
    stage       TEXT NOT NULL,
    closed_at   TEXT
);
`

// forbiddenKeywords blocks anything beyond a read-only SELECT — the LLM's output is SQL that runs
// directly against the database, so a prompt-injected or hallucinated DROP/DELETE must never reach it.
var forbiddenKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "REPLACE",
	"ATTACH", "DETACH", "PRAGMA", "TRUNCATE", "VACUUM", "REINDEX",
}

const maxRows = 50

func main() {
	app := gofr.New()

	if err := seedDB(app.GetSQL()); err != nil {
		app.Logger().Fatalf("sql-agent: seeding database failed: %v", err)
	}

	app.POST("/query", query)
	app.GET("/schema", schema)

	app.Run()
}

func query(c *gofr.Context) (any, error) {
	var in struct {
		Question string `json:"question"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	resp, err := c.LLM().Generate(c, sqlPrompt(in.Question), ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	stmt := cleanSQL(resp.Content)

	if !isSafeSelect(stmt) {
		return map[string]any{
			"question": in.Question,
			"sql":      stmt,
			"error":    "generated query was not a single read-only SELECT and was refused",
		}, nil
	}

	stmt = withLimit(stmt)

	rows, err := c.SQL.QueryContext(c, stmt)
	if err != nil {
		return map[string]any{"question": in.Question, "sql": stmt, "error": err.Error()}, nil
	}
	defer rows.Close()

	cols, results, err := scanRows(rows)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"question":  in.Question,
		"sql":       stmt,
		"columns":   cols,
		"rows":      results,
		"row_count": len(results),
		"answer":    narrate(c, in.Question, results),
	}, nil
}

// schema exposes the live table definitions the agent generates SQL against — useful for a caller
// deciding what's askable, and for sanity-checking the seeded dataset.
func schema(c *gofr.Context) (any, error) {
	rows, err := c.SQL.QueryContext(c,
		"SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	_, tables, err := scanRows(rows)
	if err != nil {
		return nil, err
	}

	return map[string]any{"dialect": c.SQL.Dialect(), "tables": tables}, nil
}

func sqlPrompt(question string) string {
	return "You translate a natural-language question into ONE read-only SQLite SELECT query over " +
		"exactly this schema:\n" + schemaSQL + "\n" +
		"Rules: reply with ONLY the SQL, no markdown fences, no explanation, no trailing semicolon " +
		"needed. Never use anything other than SELECT — no INSERT/UPDATE/DELETE/DDL. Only reference " +
		"the tables and columns above.\n\nQuestion: " + question
}

// narrate turns the raw result rows into a short, grounded answer. It is best-effort: if the model
// call fails, the caller still has the SQL and rows to work with.
func narrate(c *gofr.Context, question string, rows []map[string]any) string {
	data, err := json.Marshal(rows)
	if err != nil {
		data = []byte("[]")
	}

	resp, err := c.LLM().Generate(c,
		"Answer the question using ONLY the JSON query results below — cite concrete numbers/names "+
			"from the data. One or two sentences. Say so plainly if the results are empty.\n\n"+
			"Question: "+question+"\nResults: "+string(data),
		ai.WithTemperature(0.2))
	if err != nil {
		return fmt.Sprintf("%d row(s) returned.", len(rows))
	}

	return strings.TrimSpace(resp.Content)
}

// cleanSQL strips markdown code fences models sometimes wrap SQL in, despite being told not to.
func cleanSQL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```sql")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")

	return strings.TrimSpace(s)
}

// isSafeSelect is the guardrail: a single statement, starting with SELECT, containing none of the
// keywords that would mutate the database or its schema.
func isSafeSelect(stmt string) bool {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if trimmed == "" || strings.Contains(trimmed, ";") {
		return false
	}

	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT") {
		return false
	}

	for _, kw := range forbiddenKeywords {
		if strings.Contains(upper, kw) {
			return false
		}
	}

	return true
}

// withLimit caps unbounded result sets so a broad question can't dump an entire large table.
func withLimit(stmt string) string {
	if strings.Contains(strings.ToUpper(stmt), "LIMIT") {
		return stmt
	}

	return fmt.Sprintf("%s LIMIT %d", stmt, maxRows)
}

// scanRows reads a *sql.Rows into column names + generic rows, decoding driver []byte values (how
// modernc.org/sqlite returns most column types) back into strings.
func scanRows(rows *sql.Rows) ([]string, []map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	results := make([]map[string]any, 0)

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))

		for i := range vals {
			ptrs[i] = &vals[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}

		row := make(map[string]any, len(cols))
		for i, col := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = vals[i]
			}
		}

		results = append(results, row)
	}

	return cols, results, rows.Err()
}

// seedDB creates the schema and, on a fresh database, loads a small sales dataset — enough for
// interesting cross-table questions (top rep by closed revenue, open pipeline, headcount by dept).
func seedDB(db gofrSQLDB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}

		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM departments").Scan(&count); err != nil {
		return fmt.Errorf("seed check: %w", err)
	}

	if count > 0 {
		return nil
	}

	seed := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO departments (id, name) VALUES (?, ?)", []any{1, "Sales"}},
		{"INSERT INTO departments (id, name) VALUES (?, ?)", []any{2, "Engineering"}},
		{"INSERT INTO departments (id, name) VALUES (?, ?)", []any{3, "Marketing"}},
		{"INSERT INTO departments (id, name) VALUES (?, ?)", []any{4, "Customer Success"}},

		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{1, "Ava Chen", 1, "Account Executive", "2023-02-01", 95000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{2, "Liam Brooks", 1, "Account Executive", "2022-11-15", 98000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{3, "Noah Patel", 1, "Sales Manager", "2021-06-01", 130000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{4, "Maya Rossi", 2, "Backend Engineer", "2022-01-10", 145000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{5, "Ethan Kim", 2, "Backend Engineer", "2023-05-20", 138000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{6, "Grace Lopez", 3, "Marketing Lead", "2021-09-01", 110000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{7, "Owen Silva", 4, "Customer Success Manager", "2022-03-14", 88000}},
		{"INSERT INTO employees (id, name, department_id, role, hire_date, salary) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{8, "Zara Ahmed", 1, "Account Executive", "2024-01-08", 91000}},

		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{1, 1, "Initech", 42000, "closed_won", "2026-01-15"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{2, 1, "Globex", 18000, "closed_lost", nil}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{3, 2, "Umbrella Corp", 65000, "closed_won", "2026-02-20"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{4, 2, "Wayne Enterprises", 30000, "open", nil}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{5, 3, "Stark Industries", 120000, "closed_won", "2026-01-30"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{6, 8, "Hooli", 22000, "closed_won", "2026-03-05"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{7, 8, "Soylent", 15000, "open", nil}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{8, 1, "Aperture Science", 54000, "closed_won", "2026-03-18"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{9, 2, "Cyberdyne", 27000, "closed_lost", nil}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{10, 3, "Tyrell Corp", 88000, "closed_won", "2026-02-01"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{11, 8, "Massive Dynamic", 33000, "open", nil}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{12, 3, "Oscorp", 47000, "closed_won", "2026-03-22"}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{13, 1, "Wonka Industries", 19000, "open", nil}},
		{"INSERT INTO deals (id, employee_id, customer, amount, stage, closed_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{14, 2, "Gringotts", 72000, "closed_won", "2026-03-10"}},
	}

	for _, s := range seed {
		if _, err := db.Exec(s.query, s.args...); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	return nil
}

// gofrSQLDB is the slice of container.DB seedDB needs — kept narrow so seeding doesn't have to
// import the container package just to name the type app.GetSQL() returns.
type gofrSQLDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}
