package main

import (
	"encoding/json"
	"testing"
)

// taskFixture строит задачу в формате, который возвращает listTasks.
func taskFixture(id, uri string, createTime int64) map[string]interface{} {
	return map[string]interface{}{
		"id": id,
		"additional": map[string]interface{}{
			"detail": map[string]interface{}{
				"uri":         uri,
				"create_time": float64(createTime),
			},
		},
	}
}

func TestParseCreatedTaskID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"id as array", `{"data":{"id":["dbid_123"]},"success":true}`, "dbid_123"},
		{"id as string", `{"data":{"id":"dbid_123"},"success":true}`, "dbid_123"},
		{"no id", `{"success":true}`, ""},
		{"error response", `{"error":{"code":103},"success":false}`, ""},
		{"empty array", `{"data":{"id":[]},"success":true}`, ""},
		{"id as number", `{"data":{"id":123},"success":true}`, ""},
		{"array first element not a string", `{"data":{"id":[7,"dbid_123"]},"success":true}`, ""},
		{"broken json", `not json`, ""},
		{"empty body", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCreatedTaskID(c.body); got != c.want {
				t.Errorf("parseCreatedTaskID(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

func TestSelectCreatedTaskID(t *testing.T) {
	const uri = "magnet:?xt=urn:btih:abc&dn=test"
	const since = int64(1700000000)

	t.Run("single match", func(t *testing.T) {
		tasks := []map[string]interface{}{
			taskFixture("dbid_old", "magnet:?xt=urn:btih:other", since),
			taskFixture("dbid_new", uri, since+10),
		}
		if got := selectCreatedTaskID(tasks, uri, since); got != "dbid_new" {
			t.Errorf("selectCreatedTaskID = %q, want %q", got, "dbid_new")
		}
	})

	t.Run("uri matches but create_time older than threshold", func(t *testing.T) {
		tasks := []map[string]interface{}{taskFixture("dbid_old", uri, since-1)}
		if got := selectCreatedTaskID(tasks, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})

	t.Run("two matching tasks — ambiguous", func(t *testing.T) {
		tasks := []map[string]interface{}{
			taskFixture("dbid_a", uri, since),
			taskFixture("dbid_b", uri, since+5),
		}
		if got := selectCreatedTaskID(tasks, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})

	t.Run("create_time exactly equal to since is included (>= is inclusive)", func(t *testing.T) {
		tasks := []map[string]interface{}{taskFixture("dbid_new", uri, since)}
		if got := selectCreatedTaskID(tasks, uri, since); got != "dbid_new" {
			t.Errorf("selectCreatedTaskID = %q, want %q", got, "dbid_new")
		}
	})

	t.Run("two entries with the same id — not ambiguous", func(t *testing.T) {
		tasks := []map[string]interface{}{
			taskFixture("dbid_same", uri, since),
			taskFixture("dbid_same", uri, since+1),
		}
		if got := selectCreatedTaskID(tasks, uri, since); got != "dbid_same" {
			t.Errorf("selectCreatedTaskID = %q, want %q", got, "dbid_same")
		}
	})

	t.Run("missing create_time key — skipped safely", func(t *testing.T) {
		tasks := []map[string]interface{}{
			{
				"id": "dbid_new",
				"additional": map[string]interface{}{
					"detail": map[string]interface{}{
						"uri": uri,
					},
				},
			},
		}
		if got := selectCreatedTaskID(tasks, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})

	t.Run("non-numeric create_time — skipped safely", func(t *testing.T) {
		tasks := []map[string]interface{}{
			{
				"id": "dbid_new",
				"additional": map[string]interface{}{
					"detail": map[string]interface{}{
						"uri":         uri,
						"create_time": "123",
					},
				},
			},
		}
		if got := selectCreatedTaskID(tasks, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})

	t.Run("missing id field — no usable id", func(t *testing.T) {
		tasks := []map[string]interface{}{
			{
				"additional": map[string]interface{}{
					"detail": map[string]interface{}{
						"uri":         uri,
						"create_time": float64(since + 1),
					},
				},
			},
		}
		if got := selectCreatedTaskID(tasks, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})

	t.Run("no matching uri", func(t *testing.T) {
		tasks := []map[string]interface{}{taskFixture("dbid_x", "magnet:?xt=urn:btih:other", since)}
		if got := selectCreatedTaskID(tasks, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})

	t.Run("empty task list", func(t *testing.T) {
		if got := selectCreatedTaskID(nil, uri, since); got != "" {
			t.Errorf("selectCreatedTaskID = %q, want empty", got)
		}
	})
}

// TestCreateAuthorizedAdditionalJSON проверяет значение additional, которое
// addMagnetToNAS отправляет при create: create_authorized=true,
// seed_after_complete=false внутри download_detail.
func TestCreateAuthorizedAdditionalJSON(t *testing.T) {
	const want = `{"download_detail":{"create_authorized":true,"seed_after_complete":false}}`
	if got := createAuthorizedAdditionalJSON(); got != want {
		t.Fatalf("createAuthorizedAdditionalJSON() = %q, want %q", got, want)
	}
	var parsed map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(createAuthorizedAdditionalJSON()), &parsed); err != nil {
		t.Fatalf("additional не валидный JSON: %v (%q)", err, createAuthorizedAdditionalJSON())
	}
	details, ok := parsed["download_detail"]
	if !ok {
		t.Fatalf("нет ключа download_detail: %q", createAuthorizedAdditionalJSON())
	}
	if v, ok := details["create_authorized"].(bool); !ok || !v {
		t.Errorf("create_authorized = %v, want true", details["create_authorized"])
	}
	if v, ok := details["seed_after_complete"].(bool); !ok || v {
		t.Errorf("seed_after_complete = %v, want false", details["seed_after_complete"])
	}
}
