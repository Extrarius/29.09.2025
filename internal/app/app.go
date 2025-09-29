package app

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Extrarius/29.09.2025/internal/core"
	"github.com/Extrarius/29.09.2025/internal/downloader"
	"github.com/Extrarius/29.09.2025/internal/queue"
	"github.com/Extrarius/29.09.2025/internal/store"
)

type Config struct {
	Port            string
	DataDir         string
	DownloadDir     string
	Workers         int
	HostConcurrency int
	ClientTimeout   time.Duration
	Retries         int
	ShutdownWait    time.Duration
}

func (c *Config) Addr() string {
	if c.Port == "" {
		return ":8080"
	}
	if c.Port[0] == ':' {
		return c.Port
	}
	return ":" + c.Port
}

type App struct {
	Conf       Config
	wal        *store.WAL
	mu         sync.RWMutex
	tasks      map[string]*core.Task
	dispatcher *queue.Dispatcher
	workersWg  sync.WaitGroup
	loader     *downloader.Downloader
}

// New инициализирует приложение с заданной конфигурацией.
//
// Побочные эффекты:
//   - Создаёт каталоги conf.DataDir и conf.DownloadDir (0755).
//   - Открывает WAL в conf.DataDir.
//   - Восстанавливает незавершённые задачи из WAL (recoverFromWAL).
//   - Настраивает диспетчер очереди и HTTP-загрузчик.
//   - Запускает не менее одного фонового воркера (conf.Workers, минимум 1).
//
// Возвращает готовый *App (не забудьте вызвать Close())
// или ошибку при создании каталогов, открытии WAL либо восстановлении состояния.
// Поля конфигурации используются так:
//   - ClientTimeout, Retries, HostConcurrency — параметры загрузчика;
//   - Workers — число фоновых воркеров (min=1).
func New(conf Config) (*App, error) {
	if err := os.MkdirAll(conf.DataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(conf.DownloadDir, 0o755); err != nil {
		return nil, err
	}

	wal, err := store.OpenWAL(conf.DataDir)
	if err != nil {
		return nil, err
	}

	a := &App{
		Conf:       conf,
		wal:        wal,
		tasks:      make(map[string]*core.Task, 128),
		dispatcher: queue.NewDispatcher(10_000, 1024),
		loader: downloader.NewDownloader(downloader.Options{
			ClientTimeout:   conf.ClientTimeout,
			Retries:         conf.Retries,
			HostConcurrency: conf.HostConcurrency,
		}),
	}
	if err := a.recoverFromWAL(); err != nil {
		return nil, err
	}

	for i := 0; i < max(1, conf.Workers); i++ {
		a.workersWg.Add(1)
		go a.workerLoop(i)
	}
	return a, nil
}

// Close выполняет корректное завершение приложения.
// Останавливает диспетчер (закрывает очередь), дожидается завершения
// всех воркеров и закрывает WAL. Блокирует до полного завершения.
// Возвращает ошибку только от закрытия WAL. Обычно вызывается через defer.
func (a *App) Close() error {
	a.dispatcher.Close()
	a.workersWg.Wait()
	return a.wal.Close()
}

// Управление «дренажем» очереди (пауза/возобновление выдачи задач).
func (a *App) SetDrain(on bool) { a.dispatcher.Drain(on) }
func (a *App) IsDrain() bool    { return a.dispatcher.IsDrain() }

// recoverFromWAL восстанавливает состояние задач после перезапуска.
//
// Делает следующее:
//   - читает сохранённые задачи из WAL;
//   - все файлы со статусом Running помечает как Pending
//     (сброс ошибки и временных меток);
//   - пересчитывает статус задачи (RecomputeStatus) и кладёт её в a.tasks;
//   - повторно ставит в очередь все Pending-файлы.
//
// Вызывать до старта воркеров. Возвращает ошибку, если чтение WAL не удалось.
func (a *App) recoverFromWAL() error {
	tasks, err := a.wal.RecoverTasks()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		for _, f := range t.Files {
			if f.State == core.FileRunning {
				f.State = core.FilePending
				f.Error = ""
				f.StartedAt = nil
				f.FinishedAt = nil
			}
		}
		t.RecomputeStatus()
		a.tasks[t.ID] = t
		for i, f := range t.Files {
			if f.State == core.FilePending {
				a.dispatcher.InChan() <- queue.Job{TaskID: t.ID, FileIndex: i, Host: f.Host}
			}
		}
	}
	return nil
}

// AddTask регистрирует новую задачу, отражает её в WAL
// и ставит в очередь все файлы со статусом Pending.
//
// Шаги:
//  1. Потокобезопасно добавляет t в карту a.tasks.
//  2. Пытается дописать задачу в WAL (ошибка намеренно игнорируется).
//  3. Если t.DestDir относительный — нормализует его через filepath.Clean.
//  4. Для каждого Pending-файла публикует job в диспетчер (в канал InChan).
//
// Запись в очередь может блокировать при заполненном канале.
// Функция не возвращает ошибку.
func (a *App) AddTask(t *core.Task) {
	a.mu.Lock()
	a.tasks[t.ID] = t
	a.mu.Unlock()

	_ = a.wal.AppendTask(t)

	if !filepath.IsAbs(t.DestDir) {
		t.DestDir = filepath.Clean(t.DestDir)
	}

	for i, f := range t.Files {
		if f.State == core.FilePending {
			a.dispatcher.InChan() <- queue.Job{TaskID: t.ID, FileIndex: i, Host: f.Host}
		}
	}
}

// GetTask возвращает задачу по её ID из памяти.
// Второе значение (ok) показывает, найдена ли задача.
// Потокобезопасно читает карту задач под RLock.
// ВАЖНО: возвращается указатель на «живой» объект.
func (a *App) GetTask(id string) (*core.Task, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.tasks[id]
	return t, ok
}

// ListTasks возвращает срез всех задач из памяти.
// Чтение выполняется под RLock. Порядок не гарантируется (итерация по map).
// Возвращаются указатели на «живые» объекты.
func (a *App) ListTasks() []*core.Task {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*core.Task, 0, len(a.tasks))
	for _, t := range a.tasks {
		out = append(out, t)
	}
	return out
}

// workerLoop — основная петля фонового воркера.
//
// Читает задания из dispatcher.OutChan() до закрытия канала.
// Для каждого job:
//   - Под мьютексом валидирует задачу/индекс файла; если файл Pending —
//     переводит его в Running, сбрасывает ошибку, ставит StartedAt,
//     пересчитывает статус; фиксирует состояние в WAL.
//   - Определяет путь сохранения (t.DestDir или Conf.DownloadDir/<taskID>)
//     и делает uniquePath, чтобы не перезаписать существующий файл.
//   - Качает через loader.Fetch с контекстом (ClientTimeout*2).
//   - Под мьютексом отмечает результат: Done/Failed, BytesDownloaded,
//     ставит FinishedAt, пересчитывает статус; фиксирует в WAL.
//   - Если была ошибка и Attempts < MaxAttempts — сбрасывает файл обратно в Pending,
//     чистит таймстемпы, фиксирует в WAL и повторно публикует job в очередь.
//
// Завершение: при закрытии OutChan цикл выходит; workersWg.Done()
// сигнализирует, что воркер завершился. Ошибки записи в WAL игнорируются (best-effort).
func (a *App) workerLoop(idx int) {
	defer a.workersWg.Done()
	for job := range a.dispatcher.OutChan() {
		a.mu.Lock()
		t, ok := a.tasks[job.TaskID]
		if !ok || job.FileIndex < 0 || job.FileIndex >= len(t.Files) {
			a.mu.Unlock()
			continue
		}
		fi := t.Files[job.FileIndex]
		if fi.State != core.FilePending {
			a.mu.Unlock()
			continue
		}
		now := time.Now().UTC()
		fi.State = core.FileRunning
		fi.Error = ""
		fi.StartedAt = &now
		t.RecomputeStatus()
		a.mu.Unlock()

		_ = a.wal.AppendTask(t)

		destDir := t.DestDir
		if destDir == "" {
			destDir = filepath.Join(a.Conf.DownloadDir, t.ID)
		}
		destPath := uniquePath(filepath.Join(destDir, fi.Filename))

		ctx, cancel := context.WithTimeout(context.Background(), a.Conf.ClientTimeout*2)
		written, err := a.loader.Fetch(ctx, fi.URL, destPath)
		cancel()

		a.mu.Lock()
		now2 := time.Now().UTC()
		fi.Attempts++
		if err != nil {
			fi.State = core.FileFailed
			fi.Error = err.Error()
			fi.FinishedAt = &now2
		} else {
			fi.State = core.FileDone
			fi.Error = ""
			fi.BytesDownloaded = written
			fi.FinishedAt = &now2
		}
		t.RecomputeStatus()
		a.mu.Unlock()

		_ = a.wal.AppendTask(t)

		if err != nil && fi.Attempts < fi.MaxAttempts {
			a.mu.Lock()
			fi.State = core.FilePending
			fi.Error = ""
			fi.StartedAt = nil
			fi.FinishedAt = nil
			t.RecomputeStatus()
			a.mu.Unlock()

			_ = a.wal.AppendTask(t)

			a.dispatcher.InChan() <- queue.Job{TaskID: job.TaskID, FileIndex: job.FileIndex, Host: fi.Host}
		}
	}
}

// uniquePath возвращает уникальный путь на основе base.
// Если base не занят — возвращает его. Иначе подставляет суффикс "-N"
// перед расширением (name-1.ext, name-2.ext, …) и ищет первый свободный
// до 9999. Если не нашёл — возвращает base + "-dup".
func uniquePath(base string) string {
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		return base
	}
	ext := filepath.Ext(base)
	name := base[:len(base)-len(ext)]
	for i := 1; i < 10000; i++ {
		p := name + "-" + itoa(i) + ext
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
	return base + "-dup"
}

// itoa преобразует целое n в строку, используя стековый буфер.
// Поддерживает отрицательные значения и ноль; 20 байт хватает для int до 64 бит
// (19 цифр + знак). ВАЖНО: для math.MinInt текущее n = -n переполнится.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Serve запускает HTTP-сервер и блокируется до его остановки.
//
// Поведение:
//   - слушает адрес из a.Conf.Addr();
//   - параллельно ждёт SIGINT/SIGTERM и при получении делает graceful shutdown:
//     srv.Shutdown(ctx с таймаутом a.Conf.ShutdownWait, по умолчанию 20s) + a.Close();
//   - если ListenAndServe завершился не http.ErrServerClosed — возвращает эту ошибку;
//     при штатном завершении возвращает nil.
//
// Ошибки закрытия (Shutdown/Close) логируются, но не пробрасываются.
func (a *App) Serve(handler http.Handler) error {
	srv := &http.Server{Addr: a.Conf.Addr(), Handler: handler}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("HTTP: listen on %s", a.Conf.Addr())
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case sig := <-sigCh:
		log.Printf("Signal: %v — graceful shutdown", sig)
		wait := a.Conf.ShutdownWait
		if wait <= 0 {
			wait = 20 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		_ = srv.Shutdown(ctx)
		closeErr := a.Close()
		if closeErr != nil {
			log.Printf("close error: %v", closeErr)
		}
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
