package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Extrarius/29.09.2025/internal/app"
	"github.com/Extrarius/29.09.2025/internal/core"
)

// NewRouter собирает HTTP-маршрутизатор (http.ServeMux) для API сервиса.
//
// Эндпоинты:
//
//	GET  /healthz        — проверка живости, отвечает "ok".
//	POST /admin/drain    — поставить диспетчер на «паузу» (drain=true).
//	POST /admin/resume   — снять «паузу» (drain=false).
//	POST /tasks          — создать задачу: {links, label, dest_dir}; возвращает {task_id}.
//	GET  /tasks          — список всех задач (в памяти).
//	GET  /tasks/{id}     — данные одной задачи.
//
// Примечания:
//   - dest_dir (если задан) присоединяется под a.Conf.DownloadDir.
//   - ошибки сериализуются в HTTP-коды/сообщения.
//   - обработчик обёрнут в withRecover(mux) для защиты от паник.
func NewRouter(a *app.App) http.Handler {
	mux := http.NewServeMux()

	// health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// admin
	mux.HandleFunc("/admin/drain", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		a.SetDrain(true)
		writeJSON(w, map[string]any{"drain": true})
	})
	mux.HandleFunc("/admin/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		a.SetDrain(false)
		writeJSON(w, map[string]any{"drain": false})
	})

	// tasks
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Links   []string `json:"links"`
				Label   string   `json:"label"`
				DestDir string   `json:"dest_dir"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
				return
			}
			if len(req.Links) == 0 {
				http.Error(w, "links must be non-empty", http.StatusBadRequest)
				return
			}
			task, err := core.NewTask(req.Label, req.DestDir, req.Links, a.Conf.Retries)
			if err != nil {
				http.Error(w, "invalid task: "+err.Error(), http.StatusBadRequest)
				return
			}
			if task.DestDir == "" {
				task.DestDir = filepath.Join(a.Conf.DownloadDir, task.ID)
			} else {
				task.DestDir = filepath.Join(a.Conf.DownloadDir, task.DestDir)
			}
			a.AddTask(task)
			writeJSON(w, map[string]string{"task_id": task.ID})
		case http.MethodGet:
			limit, _ := positiveInt(r, "limit", 100)
			offset, _ := positiveInt(r, "offset", 0)

			tasks := a.ListTasks()
			if offset > len(tasks) {
				offset = len(tasks)
			}
			end := offset + limit
			if end > len(tasks) {
				end = len(tasks)
			}

			writeJSON(w, tasks[offset:end])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// task by id
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/tasks/")
		if id == "" || strings.ContainsRune(id, '/') {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		t, ok := a.GetTask(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, t)
	})

	return withRecover(mux)
}

// writeJSON сериализует v в JSON с отступами и пишет в ответ,
// устанавливая Content-Type: application/json; charset=utf-8.
// Ошибка кодирования игнорируется.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// withRecover — middleware, которое перехватывает panic в обработчиках,
// не даёт упасть всему серверу и возвращает 500 Internal Server Error.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				http.Error(w, fmt.Sprintf("internal error: %v", p), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// positiveInt читает из query-параметров r значение по ключу key,
// парсит его как неотрицательное целое и возвращает.
// Если параметр отсутствует — возвращает def без ошибки.
// Если значение не число или < 0 — возвращает ошибку.
func positiveInt(r *http.Request, key string, def int) (int, error) {
	if s := r.URL.Query().Get(key); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, errors.New("bad int for " + key)
		}
		return n, nil
	}
	return def, nil
}
