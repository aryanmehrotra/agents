// surreal.go provisions the SurrealDB namespace and database before GoFr's datasource connects, so
// a brand-new SurrealDB (e.g. a throwaway `surreal start memory`) works with zero manual setup — the
// "anyone can run it" path. It uses SurrealDB's HTTP /sql endpoint directly (root auth) because
// DEFINE NAMESPACE/DATABASE must exist before the datasource issues its USE on connect.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// provisionSurreal creates the namespace and database if they don't already exist. It is idempotent
// and best-effort: a failure (e.g. SurrealDB not up yet) is returned for the caller to log, and the
// datasource's own health check will surface a persistent problem.
func provisionSurreal(ctx context.Context, host string, port int, user, pass, ns, db string) error {
	endpoint := fmt.Sprintf("http://%s:%d/sql", host, port)
	client := &http.Client{Timeout: 10 * time.Second}

	do := func(nsHeader, sql string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(sql))
		if err != nil {
			return err
		}

		req.SetBasicAuth(user, pass)
		req.Header.Set("Accept", "application/json")

		if nsHeader != "" {
			req.Header.Set("surreal-ns", nsHeader)
		}

		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= http.StatusMultipleChoices {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("surreal /sql returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		return nil
	}

	if err := do("", "DEFINE NAMESPACE IF NOT EXISTS "+ns+";"); err != nil {
		return fmt.Errorf("define namespace %q: %w", ns, err)
	}

	if err := do(ns, "DEFINE DATABASE IF NOT EXISTS "+db+";"); err != nil {
		return fmt.Errorf("define database %q: %w", db, err)
	}

	return nil
}
