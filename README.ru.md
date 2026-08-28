# CodexPC Connector

[English](README.md) | [Русский](README.ru.md)

> Локальный MCP-адаптер для Codex app-server с защищённым доступом к файловой системе, управляемым запуском процессов и маршрутизацией к другим MCP-серверам.

[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/Protocol-MCP-111827)](https://modelcontextprotocol.io/)
[![License: MIT](https://img.shields.io/badge/License-MIT-22C55E.svg)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/niktoimiyazap/codex-mcp-router/test.yml?branch=main&label=tests)](https://github.com/niktoimiyazap/codex-mcp-router/actions)

## Возможности

CodexPC Connector предоставляет MCP-клиентам контролируемый набор локальных инструментов:

- безопасное чтение и изменение файлов;
- атомарная запись UTF-8 с защитой от конфликтов;
- синхронный и фоновый запуск процессов;
- тайм-ауты, отмена, ограничение вывода и завершение дерева процессов;
- просмотр, поиск и вызов инструментов других MCP-серверов через Codex app-server;
- структурированные логи с удалением секретов и защита от запуска нескольких экземпляров.

## Архитектура

```text
MCP-клиент
    |
    v
CodexPC Connector
    |-- политика доступа к файлам и проверка UTF-8
    |-- управляемые локальные процессы
    `-- клиент JSON-RPC / JSONL
             |
             v
       codex app-server
         |-- fs/*
         |-- mcpServerStatus/list
         `-- mcpServer/tool/call
```

Коннектор запускает один долгоживущий процесс `codex app-server --stdio` и создаёт временный поток Codex для поиска и вызова MCP-инструментов.

## Требования

- Go 1.26 или новее — только для сборки из исходников;
- Codex CLI с поддержкой `codex app-server`;
- выполненный вход в Codex;
- настроенные в Codex MCP-серверы, если нужна дальнейшая маршрутизация.

## Быстрый старт

```bash
git clone https://github.com/niktoimiyazap/codex-mcp-router.git
cd codex-mcp-router
go build -o dist/codexpc-go.exe ./cmd/codexpc
```

Рекомендуемая сборка и запуск на Windows:

```bat
build-go.cmd
wrapper.cmd
```

`build-go.cmd` выполняет форматирование, тесты, сборку, smoke-тест, обновляет `dist\codexpc-go.exe` и по возможности копирует свежий бинарник на рабочий стол.

Старая Python-реализация сохранена только в `archive/python-legacy/` для истории и регрессионных сравнений.

## Интерактивный запуск туннеля

Чтобы подключить локальный MCP-сервер к уже созданному туннелю OpenAI:

```bat
launch-tunnel.cmd
```

На macOS:

```bash
chmod +x launch-tunnel.sh
./launch-tunnel.sh
```

При первом запуске мастер запрашивает данные туннеля и сохраняет Runtime API key в Windows Credential Manager или macOS Keychain. При следующих запусках ключ используется автоматически. Подробнее: [настройка туннеля](docs/TUNNEL_SETUP.md).

## Конфигурация

Скопируйте `config.example.toml` в системную папку конфигурации:

| Платформа | Путь |
|---|---|
| Windows | `%LOCALAPPDATA%\CodexPCConnector\config.toml` |
| macOS | `~/Library/Application Support/CodexPCConnector/config.toml` |
| Linux | `$XDG_STATE_HOME/codexpc-connector/config.toml` |

Минимальный пример:

```toml
workspace = "~/projects"
allowed_roots = ["~/projects"]

tool_profile = "core"

```

Коннектор теперь специально сделан тонким: файловые операции и терминал выполняет оригинальный Codex app-server. В коннекторе остаются проверка разрешённых путей, MCP-маршрутизация, нормализация ответов и нативное управление Windows.

Все параметры описаны в [документации по конфигурации](docs/CONFIGURATION.md).

## Группы инструментов

### Файловая система

`fs_read_file`, `fs_write_file`, `fs_read_directory`, `fs_create_directory`, `fs_copy`, `fs_remove`

Это тонкие адаптеры над оригинальными методами Codex `fs/*`. Собственный патчер, снапшоты файлов, обходчик проекта и прямые файловые операции из коннектора удалены.

### Терминал

`command_exec`

Команды передаются argv-массивом в оригинальный метод Codex `command/exec`. Собственного менеджера процессов и фоновых задач в коннекторе больше нет.

### Управление компьютером

`computer`

Нативные скриншоты, мышь, клавиатура и прокрутка Windows остаются локальными.

### MCP-маршрутизация

`mcp_discover`, `mcp_call`

`mcp_discover` выводит или ищет настроенные MCP-серверы и инструменты через единый постоянный кэш инвентаря. Устаревшие данные возвращаются сразу, пока фоновое обновление получает актуальный список. Профиль `full` дополнительно выставляет совместимые псевдонимы `mcp_list_servers`, `mcp_list_tools` и `mcp_search_tools`.

### Управление коннектором

`connector_status`, `list_active_tool_calls`, `cancel_tool_calls`

## Проверка

```bash
go fmt ./cmd/... ./internal/...
go test ./...
go build -trimpath -o dist/codexpc-go.exe ./cmd/codexpc
```

На Windows полный цикл проверки выполняется одной командой:

```bat
build-go.cmd
```

## Документация

- [Архитектура](docs/ARCHITECTURE.md)
- [Конфигурация](docs/CONFIGURATION.md)
- [Настройка туннеля](docs/TUNNEL_SETUP.md)
- [Политика безопасности](SECURITY.md)
- [Как внести вклад](CONTRIBUTING.md)
- [Процесс релиза](docs/RELEASING.md)
- [История изменений](CHANGELOG.md)

## Безопасность

Это привилегированное локальное ПО для одного доверенного пользователя, работающее через MCP stdio. Не публикуйте его напрямую в сети. Перед включением запуска процессов или shell-команд ознакомьтесь с [SECURITY.md](SECURITY.md).

## Лицензия

MIT
