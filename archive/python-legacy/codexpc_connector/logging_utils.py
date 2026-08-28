from __future__ import annotations

import json
import logging
import queue
from logging.handlers import QueueHandler, QueueListener, RotatingFileHandler
from pathlib import Path
from typing import Any

from .config import Settings
from .security import redact

_LISTENERS: dict[str, QueueListener] = {}


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
            "time": self.formatTime(record, "%Y-%m-%dT%H:%M:%S"),
        }
        extra = getattr(record, "event_data", None)
        if extra is not None:
            payload["data"] = redact(extra)
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(redact(payload), ensure_ascii=False)


def configure_logging(settings: Settings) -> logging.Logger:
    settings.log_dir.mkdir(parents=True, exist_ok=True)
    logger = logging.getLogger("codexpc")
    close_logging(logger)
    logger.setLevel(getattr(logging, settings.log_level, logging.INFO))
    logger.propagate = False

    file_handler = RotatingFileHandler(
        Path(settings.log_dir) / "connector.jsonl",
        maxBytes=5 * 1024 * 1024,
        backupCount=3,
        encoding="utf-8",
    )
    file_handler.setFormatter(JsonFormatter())
    log_queue: queue.SimpleQueue[logging.LogRecord] = queue.SimpleQueue()
    queue_handler = QueueHandler(log_queue)
    logger.addHandler(queue_handler)

    listener = QueueListener(log_queue, file_handler, respect_handler_level=True)
    listener.start()
    _LISTENERS[logger.name] = listener
    return logger


def close_logging(logger: logging.Logger) -> None:
    listener = _LISTENERS.pop(logger.name, None)
    if listener is not None:
        listener.stop()
        for handler in listener.handlers:
            handler.flush()
            handler.close()
    for handler in list(logger.handlers):
        handler.flush()
        handler.close()
        logger.removeHandler(handler)


def log_event(logger: logging.Logger, level: int, message: str, **data: Any) -> None:
    logger.log(level, message, extra={"event_data": data})
