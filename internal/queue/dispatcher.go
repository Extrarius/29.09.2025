package queue

import (
	"sync"
	"sync/atomic"
	"time"
)

// Диспетчер: принимает Job в InChan, отдаёт воркерам из OutChan.
// Поддерживает drain (пауза выдачи новых работ) и backlog.
type Job struct {
	TaskID    string
	FileIndex int
	Host      string
}

type Dispatcher struct {
	jobInCh chan Job
	taskCh  chan Job
	backlog []Job
	mu      sync.Mutex
	drain   atomic.Bool
	closed  atomic.Bool

	flushTicker *time.Ticker
	stopCh      chan struct{}
}

// NewDispatcher создаёт диспетчер очереди заданий.
//
// Параметры:
//
//	inBuffer     — ёмкость входного канала (сколько задач можно положить,
//	               не блокируясь, пока планировщик не подхватит их);
//	workerBuffer — ёмкость выходного канала для воркеров.
//
// Инициализирует внутренний backlog (предварительный буфер),
// тиканье flushTicker каждые ~250ms и goroutine планировщика (schedulerLoop),
// которая периодически переливает задания из backlog в выходной канал.
// Возвращает готовый *Dispatcher; остановка — через d.Close().
func NewDispatcher(inBuffer, workerBuffer int) *Dispatcher {
	d := &Dispatcher{
		jobInCh:     make(chan Job, inBuffer),
		taskCh:      make(chan Job, workerBuffer),
		backlog:     make([]Job, 0, 1024),
		flushTicker: time.NewTicker(250 * time.Millisecond),
		stopCh:      make(chan struct{}),
	}
	go d.schedulerLoop()
	return d
}

// Close останавливает диспетчер.
// Идемпотентна: повторные вызовы ничего не делают (closed.Swap(true)).
// Действия:
//   - посылает сигнал остановки планировщику через stopCh,
//   - останавливает таймер flushTicker.
//
// Каналы jobInCh/taskCh намеренно не закрываются здесь, чтобы не ронять
// отправителей/получателей; закрытие/дренаж выполняет цикл планировщика.
func (d *Dispatcher) Close() {
	if d.closed.Swap(true) {
		return
	}
	close(d.stopCh)
	d.flushTicker.Stop()
}

// Drain включает/выключает «паузу выдачи» задач воркерам.
// При on=true диспетчер перестаёт отправлять задания в taskCh,
// накапливая их во внутреннем backlog (удобно для graceful shutdown
// или временной остановки потребителей). При on=false — возобновляет выдачу.
// Потокобезопасно, переключение выполняется атомарно и мгновенно.
func (d *Dispatcher) Drain(on bool) { d.drain.Store(on) }

// IsDrain сообщает, включён ли режим «дренажа»/паузы выдачи задач.
// true — выдача в taskCh приостановлена, задания копятся в backlog.
// Потокобезопасно: читает атомарный флаг.
func (d *Dispatcher) IsDrain() bool { return d.drain.Load() }

// InChan возвращает входной канал для постановки заданий.
// Канал только на отправку (chan<-): продюсеры пишут сюда Job,
// планировщик читает и перекладывает во внутренний backlog.
// Не закрывайте этот канал вручную; остановку выполняет Dispatcher.
// Отправка может блокировать при заполненном буфере (backpressure).
func (d *Dispatcher) InChan() chan<- Job { return d.jobInCh }

// OutChan возвращает канал выдачи задач для воркеров.
// Канал только для чтения (<-chan). Типичный паттерн:
//
//	for job := range d.OutChan() { ... }
//
// Чтение блокируется, если задач нет; закрытие/дренаж управляет планировщик.
func (d *Dispatcher) OutChan() <-chan Job { return d.taskCh }

// schedulerLoop — главный цикл диспетчера.
//
// Обрабатывает три события:
//   - <-stopCh         — завершение работы цикла;
//   - <-flushTicker.C  — периодическая попытка выдать накопленное (tryFlushBacklog);
//   - j := <-jobInCh   — поступление новой задачи.
//
// Логика при поступлении job:
//
//	– если включён Drain — кладёт job в backlog;
//	– иначе пытается неблокирующе отправить в taskCh;
//	  если taskCh полон — перемещает job в backlog.
//
// Порядок задач не строго гарантируется (из-за неблокирующей отправки и бэклога).
// Частота сброса регулируется flushTicker.
func (d *Dispatcher) schedulerLoop() {
	for {
		select {
		case <-d.stopCh:
			close(d.taskCh)
			return
		case <-d.flushTicker.C:
			d.tryFlushBacklog()
		case j := <-d.jobInCh:
			if d.IsDrain() {
				d.mu.Lock()
				d.backlog = append(d.backlog, j)
				d.mu.Unlock()
				continue
			}
			select {
			case d.taskCh <- j:
			default:
				d.mu.Lock()
				d.backlog = append(d.backlog, j)
				d.mu.Unlock()
			}
		}
	}
}

// tryFlushBacklog пытается выдать накопленные задания из backlog в taskCh.
// Ничего не делает, если включён Drain. Работает под мьютексом,
// отправляет неблокирующе (select default) и прекращает, как только taskCh полон.
// Порядок в backlog — FIFO (всегда берём первый элемент).
func (d *Dispatcher) tryFlushBacklog() {
	if d.IsDrain() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.backlog) > 0 {
		select {
		case d.taskCh <- d.backlog[0]:
			d.backlog = d.backlog[1:]
		default:
			return
		}
	}
}
