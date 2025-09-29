package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// TaskStatus — агрегированный статус задачи
type TaskStatus string

const (
	TaskPending  TaskStatus = "PENDING"
	TaskRunning  TaskStatus = "RUNNING"
	TaskComplete TaskStatus = "COMPLETE"
	TaskFailed   TaskStatus = "FAILED"
	TaskPartial  TaskStatus = "PARTIAL"
)

// FileState — статус конкретного файла
type FileState string

const (
	FilePending FileState = "PENDING"
	FileRunning FileState = "RUNNING"
	FileDone    FileState = "DONE"
	FileFailed  FileState = "FAILED"
)

// FileItem — описание одного файла
type FileItem struct {
	URL             string     `json:"url"`
	Filename        string     `json:"filename"`
	State           FileState  `json:"state"`
	Error           string     `json:"error,omitempty"`
	Attempts        int        `json:"attempts"`
	MaxAttempts     int        `json:"max_attempts"`
	BytesDownloaded int64      `json:"bytes_downloaded"`
	SizeHint        int64      `json:"size_hint,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Host            string     `json:"host"`
}

// Task — бизнес-объект задачи
type Task struct {
	ID        string      `json:"id"`
	Label     string      `json:"label,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	DestDir   string      `json:"dest_dir"`
	Status    TaskStatus  `json:"status"`
	Files     []*FileItem `json:"files"`

	Total   int `json:"total"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
	Running int `json:"running"`
	Retries int `json:"retries_total"`
}

// NewTask конструирует новую задачу скачивания из списка ссылок.
//
// Делает:
//   - валидирует вход: links не пуст, каждая ссылка парсится и имеет схему/хост;
//   - для каждой ссылки создаёт FileItem:
//     – имя файла = path.Base(URL.Path), при пустом — "file";
//     – имя проходит sanitizeFilename;
//     – начальное состояние FilePending;
//     – Host берётся из URL.Host;
//     – MaxAttempts устанавливается из аргумента;
//   - генерирует ID, заполняет Label, DestDir, CreatedAt (UTC),
//     ставит начальный статус TaskPending и вызывает RecomputeStatus.
//
// Возвращает *Task или ошибку при пустом списке/некорректной ссылке.
func NewTask(label string, destDir string, links []string, maxAttempts int) (*Task, error) {
	if len(links) == 0 {
		return nil, fmt.Errorf("пустой список ссылок")
	}
	files := make([]*FileItem, 0, len(links))
	for _, link := range links {
		u, err := url.Parse(link)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("некорректная ссылка: %q", link)
		}
		base := path.Base(u.Path)
		if base == "." || base == "/" || base == "" {
			base = "file"
		}
		files = append(files, &FileItem{
			URL:         link,
			Filename:    sanitizeFilename(base),
			State:       FilePending,
			MaxAttempts: maxAttempts,
			Host:        u.Host,
		})
	}
	id := NewID()
	t := &Task{
		ID:        id,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		DestDir:   destDir,
		Status:    TaskPending,
		Files:     files,
	}
	t.RecomputeStatus()
	return t, nil
}

// sanitizeFilename приводит произвольную строку к безопасному имени файла.
//
// Делает:
//   - отбрасывает всё после '?' (query из URL);
//   - если имя пустое, "." или "/" — возвращает "file";
//   - заменяет/удаляет опасные символы:
//     ':' '/' '\' → '-' ;  '*' '?' '"' '<' '>' '|' '\n' '\r' → удаляются.
func sanitizeFilename(s string) string {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == "." || s == "/" {
		return "file"
	}
	r := strings.NewReplacer(
		":", "-",
		"/", "-",
		"\\", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
		"\n", "",
		"\r", "",
	)
	return r.Replace(s)
}

// RecomputeStatus пересчитывает агрегаты задачи по её файлам:
// Total/Done/Failed/Pending/Running/Retries.
// По результатам устанавливает итоговый статус:
//   - TaskComplete — все файлы Done;
//   - TaskFailed   — все файлы Failed;
//   - TaskRunning  — есть хотя бы один Running;
//   - TaskPartial  — есть Done и Failed, и при этом нет Pending/Running;
//   - иначе TaskPending.
func (t *Task) RecomputeStatus() {
	total := len(t.Files)
	var done, failed, pending, running, retries int
	for _, f := range t.Files {
		switch f.State {
		case FileDone:
			done++
		case FileFailed:
			failed++
		case FilePending:
			pending++
		case FileRunning:
			running++
		}
		retries += f.Attempts
	}
	t.Total = total
	t.Done = done
	t.Failed = failed
	t.Pending = pending
	t.Running = running
	t.Retries = retries

	switch {
	case total > 0 && done == total:
		t.Status = TaskComplete
	case total > 0 && failed == total:
		t.Status = TaskFailed
	case running > 0:
		t.Status = TaskRunning
	case done > 0 && failed > 0 && pending == 0 && running == 0:
		t.Status = TaskPartial
	default:
		t.Status = TaskPending
	}
}

// NewID генерирует человекочитаемый идентификатор вида
// "YYYYMMDD-HHMMSS-xxxxxx": префикс — UTC-время (секундная точность),
// суффикс — 3 случайных байта в hex (6 символов).
// Даёт монотонно возрастающие ID и низкую вероятность коллизий
// (~1 из 16,7 млн в одну секунду). Подходит для имён файлов/задач,
// не предназначено для криптографии.
func NewID() string {
	now := time.Now().UTC().Format("20060102-150405")
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now + "-" + strings.ToLower(hex.EncodeToString(b[:]))
}
