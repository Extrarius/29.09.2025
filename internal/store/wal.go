package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Extrarius/29.09.2025/internal/core"
)

type walRecord struct {
	Type string     `json:"type"` // "upsert_task"
	Task *core.Task `json:"task,omitempty"`
}

type WAL struct {
	mu   sync.Mutex
	f    *os.File
	path string
	w    *bufio.Writer
}

// OpenWAL открывает (или создаёт) файл журнала tasks.wal в dataDir.
//
// Делает:
//   - гарантирует наличие каталога dataDir (0755);
//   - открывает файл в режимах O_CREATE|O_RDWR|O_APPEND (без truncate), права 0644;
//   - оборачивает файл буфером записи 64 KiB.
//
// Возвращает *WAL, готовый к записи. Данные буферизуются — они гарантированно
// записываются на диск при Flush/Close (вызовите Close() по завершении работы).
func OpenWAL(dataDir string) (*WAL, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "tasks.wal")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{
		f:    f,
		path: path,
		w:    bufio.NewWriterSize(f, 64*1024),
	}, nil
}

// Close завершает работу с WAL:
//
//	– под мьютексом пытается сбросить буфер (Flush);
//	– затем закрывает файловый дескриптор.
//
// Возвращает ошибку только от Close(); ошибка Flush в текущей реализации игнорируется.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.w != nil {
		w.w.Flush()
	}
	if w.f != nil {
		return w.f.Close()
	}
	return nil
}

// AppendTask добавляет в WAL одну запись типа "upsert_task" в формате JSONL.
// Потокобезопасно пишет в конец файла и выполняет Flush буфера,
// чтобы данные оказались в файле. Возвращает ошибку маршалинга/записи/Flush.
func (w *WAL) AppendTask(task *core.Task) error {
	rec := walRecord{Type: "upsert_task", Task: task}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal wal record: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(append(data, '\n')); err != nil {
		return err
	}
	return w.w.Flush()
}

// RecoverTasks перечитывает файл WAL (w.path) и восстанавливает последнее
// известное состояние задач.
//
// Формат WAL — JSONL: по одной JSON-записи на строку. Учитываются только
// записи с Type="upsert_task"; применяется политика last-write-wins — для
// каждого Task.ID в результате остаётся самое позднее встретившееся состояние.
//
// Реализация:
//   - открывает файл и сканирует построчно через bufio.Scanner;
//   - увеличивает лимит токена до 10 МБ (sc.Buffer(..., 10*1024*1024));
//   - некорректные/битые строки пропускает (continue), не прерывая восстановление;
//   - на выходе возвращает map[Task.ID]*Task или ошибку сканера.
//
// Предназначено для вызова на старте приложения, до запуска воркеров.
func (w *WAL) RecoverTasks() (map[string]*core.Task, error) {
	f, err := os.Open(w.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tasks := make(map[string]*core.Task, 128)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var rec walRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type == "upsert_task" && rec.Task != nil {
			tasks[rec.Task.ID] = rec.Task
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}
