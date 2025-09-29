# Сервис на Go для скачивания файлов по задачам

Пользователь создаёт задачу со списком ссылок, сервис скачивает файлы в указанную папку и даёт API для контроля статуса.  
Поддержаны: **перезапуск/остановка без потери задач**, **graceful shutdown**, режим **drain** (временная пауза выдачи заданий воркерам).

> **Инфраструктура:** не требуется БД/Docker/nginx — всё на локальном диске.  
> **Зависимости:** стандартная библиотека Go + `github.com/joho/godotenv` (для `.env`).

---

## Быстрый старт

### сборка бинарника
```bash
go mod tidy
cp .env
go build -o bin/downloader ./cmd/downloader
./bin/downloader
```

Проверка живости:
```bash
curl -i http://localhost:8080/healthz
```

Остановка с graceful shutdown — `Ctrl+C` (SIGINT) или SIGTERM.

---

## Конфигурация (`.env`)

Файл `.env` загружается автоматически (также читаются `.env.local` и путь из `ENV_FILE`, если задан). Пример — в `.env.example`:

```dotenv
# Сетевые параметры
PORT=8080

# Каталоги
DATA_DIR=./data
DOWNLOAD_DIR=./downloads

# Параллельность и надёжность
WORKERS=4
HOST_CONCURRENCY=2
CLIENT_TIMEOUT=30s
RETRIES=3
SHUTDOWN_WAIT=20s

# Альтернативный файл конфигурации (опционально)
# ENV_FILE=.env.local
```

> Значения по умолчанию также «зашиты» в `main.go` через хелперы `env`, `envInt`, `envDuration`.

---

## API

База: `http://localhost:${PORT:-8080}`

### Здоровье
```
GET /healthz  → 200 OK, "ok"
```

### Управление выдачей заданий (drain)
```
POST /admin/drain   → { "drain": true }   # ставим на паузу (новые задания не стартуют)
POST /admin/resume  → { "drain": false }  # снимаем с паузы
```

### Задачи
```
POST /tasks
Body: {
  "links": ["https://example.com/a.jpg", "https://example.com/b.jpg"],
  "label": "my-photos",
  "dest_dir": "album1"            # опционально; будет сохранено под DOWNLOAD_DIR/album1
}
→ 200 OK { "task_id": "20250929-101530-abcdef" }

GET /tasks
→ 200 OK [ { ...task... }, ... ]   # список (в памяти)

GET /tasks/{id}
→ 200 OK { ...task... }  |  404 Not Found
```

**Примеры `curl`:**
```bash
# создать задачу
curl -sS -X POST http://localhost:8080/tasks   -H 'Content-Type: application/json'   -d '{"links":["https://speed.hetzner.de/100MB.bin","https://speed.hetzner.de/1GB.bin"],"label":"hetzner","dest_dir":"tests"}'

# список задач
curl -sS http://localhost:8080/tasks | jq

# одна задача
curl -sS http://localhost:8080/tasks/20250929-101530-abcdef | jq

# пауза/возобновление
curl -sS -X POST http://localhost:8080/admin/drain | jq
curl -sS -X POST http://localhost:8080/admin/resume | jq
```

---

## Как это работает (коротко)

- **WAL (журнал)**: каждое обновление задачи пишется в `DATA_DIR/tasks.wal` (JSONL).  
  При старте сервис читает WAL и **восстанавливает** последние состояния задач. Все файлы, которые были в статусе *Running*, переводятся в *Pending* и перезапускаются.
- **Очередь и воркеры**: `Dispatcher` принимает задания и раздаёт их `WORKERS`-воркерам.  
  `HOST_CONCURRENCY` ограничивает одновременные загрузки с одного хоста (пер-хост семафор).
- **Надёжность**: скачивание идёт во временный файл `*.part`, затем **атомарный `rename`**. Есть ретраи `RETRIES` с экспоненциальным backoff. Таймаут HTTP — `CLIENT_TIMEOUT`.
- **Graceful shutdown**: по SIGINT/SIGTERM сервис перестаёт выдавать новые задания, ждёт выполнение текущих в рамках `SHUTDOWN_WAIT`, сохраняет состояния и закрывается.

---

## Структура проекта (ключевое)

```
cmd/downloader/         # точка входа (main)
internal/app/           # инициализация и жизненный цикл приложения
internal/http/          # HTTP API (маршруты и сериализация)
internal/queue/         # диспетчер очереди (drain/backlog/выдача)
internal/downloader/    # загрузчик HTTP с ретраями и ограничением по хостам
internal/store/         # WAL (журнал), восстановление задач
internal/core/          # доменные типы: Task, FileItem и т.д.
```

---

## Разработка

- Запуск с автоподхватом `.env`:
  ```bash
  go run ./cmd/downloader
  ```
- Примеры запросов: `examples/requests.http` (можно открывать в VS Code/GoLand HTTP Client).
- Линт/формат: `go fmt ./...` / `go vet ./...` / (по желанию) golangci-lint.
