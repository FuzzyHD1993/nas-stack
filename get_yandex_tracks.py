#!/usr/bin/env python3
"""Collect Yandex Music library metadata (likes + user playlists).

This script uses only metadata API calls. It never requests audio streams or
download links.

Authentication:
  - set YANDEX_MUSIC_TOKEN, or
  - put a token in ~/.config/yandex-music/token, or
  - run with --login to use Yandex OAuth Device Flow.

Output default: ~/yandex_music_library.json
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Sequence

from yandex_music import Client

TOKEN_PATH = Path.home() / ".config" / "yandex-music" / "token"
DEFAULT_OUTPUT = Path.home() / "yandex_music_library.json"
BATCH_SIZE = 100


def eprint(*args: Any, **kwargs: Any) -> None:
    print(*args, file=sys.stderr, flush=True)


def load_token(token_file: Path, force_login: bool) -> str:
    if not force_login:
        env_token = os.environ.get("YANDEX_MUSIC_TOKEN", "").strip()
        if env_token:
            return env_token
        if token_file.exists():
            token = token_file.read_text(encoding="utf-8").strip()
            if token:
                return token

    token_path = token_file
    token_path.parent.mkdir(parents=True, exist_ok=True)
    client = Client()
    code = client.request_device_code(device_name="OpenCode NAS")

    eprint("=" * 64)
    eprint("Яндекс.Музыка: подтвердите вход")
    eprint(f"URL:  {code.verification_url}")
    eprint(f"Код:  {code.user_code}")
    eprint(f"Код действует {code.expires_in} секунд.")
    eprint("=" * 64)

    deadline = time.monotonic() + code.expires_in
    while time.monotonic() < deadline:
        try:
            token = client.poll_device_token(code.device_code)
        except Exception as exc:  # noqa: BLE001 - retry transient polling errors
            eprint(f"Ошибка опроса OAuth: {type(exc).__name__}: {exc}")
            time.sleep(max(code.interval, 5))
            continue

        if token is not None:
            token_path.write_text(token.access_token + "\n", encoding="utf-8")
            os.chmod(token_path, 0o600)
            eprint(f"Токен сохранён: {token_path}")
            return token.access_token
        time.sleep(code.interval)

    raise SystemExit("Device Flow истёк: получите код заново и повторите --login")


def format_duration(duration_ms: int | None) -> str | None:
    if not duration_ms:
        return None
    seconds = round(duration_ms / 1000)
    minutes, seconds = divmod(seconds, 60)
    hours, minutes = divmod(minutes, 60)
    if hours:
        return f"{hours}:{minutes:02d}:{seconds:02d}"
    return f"{minutes}:{seconds:02d}"


def track_to_dict(track: Any) -> dict[str, Any]:
    artists = [artist.name for artist in (getattr(track, "artists", None) or []) if getattr(artist, "name", None)]
    albums = getattr(track, "albums", None) or []
    album = getattr(albums[0], "title", None) if albums else None
    duration_ms = getattr(track, "duration_ms", None)
    return {
        "id": str(getattr(track, "id", "")),
        "title": getattr(track, "title", None),
        "artist": ", ".join(artists) if artists else None,
        "artists": artists,
        "album": album,
        "duration_ms": duration_ms,
        "duration": format_duration(duration_ms),
        "available": getattr(track, "available", None),
        "explicit": getattr(track, "explicit", None),
        "isrc": getattr(track, "isrc", None),
    }


def short_to_dict(short: Any) -> dict[str, Any]:
    """Fallback for a track whose full object could not be fetched."""
    return {
        "id": str(getattr(short, "id", "")),
        "track_id": getattr(short, "track_id", None),
        "album_id": getattr(short, "album_id", None),
        "title": None,
        "artist": None,
        "artists": [],
        "album": None,
        "duration_ms": None,
        "duration": None,
        "available": None,
        "explicit": None,
        "isrc": None,
        "error": "full track metadata unavailable",
    }


def chunked(items: Sequence[str], size: int) -> Iterable[Sequence[str]]:
    for i in range(0, len(items), size):
        yield items[i : i + size]


def resolve_tracks(
    client: Client,
    shorts: Sequence[Any],
    *,
    per_track: bool,
    cache: dict[str, dict[str, Any]],
) -> list[dict[str, Any]]:
    """Resolve TrackShort objects to full Track metadata.

    Playlist entries often already contain a full Track object in ``short.track``
    (including tracks unavailable by region/licence). Use that first, then fall
    back to fetch_track()/client.tracks(). Default mode batches IDs through
    client.tracks() to avoid one HTTP request per track. --per-track uses
    TrackShort.fetch_track() for every track, as in the original example, but is
    much slower on large libraries.
    """
    result: list[dict[str, Any]] = []
    keys = [getattr(short, "track_id", None) or str(getattr(short, "id", "")) for short in shorts]

    # TrackShort objects from playlists usually carry the complete track payload.
    for short, key in zip(shorts, keys):
        embedded = getattr(short, "track", None)
        if embedded is not None and key not in cache:
            item = track_to_dict(embedded)
            item["metadata_source"] = "playlist_track"
            cache[key] = item

    if per_track:
        for index, (short, key) in enumerate(zip(shorts, keys), 1):
            if key in cache:
                result.append(cache[key])
                continue
            try:
                full = short.fetch_track()
                item = track_to_dict(full)
                item["metadata_source"] = "fetch_track"
            except Exception as exc:  # noqa: BLE001 - keep collection going
                eprint(f"  ! не удалось получить трек {key}: {type(exc).__name__}: {exc}")
                item = short_to_dict(short)
            cache[key] = item
            result.append(item)
            if index % 100 == 0:
                eprint(f"    обработано {index}/{len(shorts)}")
        return result

    # Batch mode: one request per 100 IDs, skipping entries embedded above.
    for batch in chunked(keys, BATCH_SIZE):
        missing = [key for key in batch if key not in cache]
        if not missing:
            continue
        try:
            full_tracks = client.tracks(list(missing))
        except Exception as exc:  # noqa: BLE001 - fall back to per-track for this batch
            eprint(f"  ! пакетный запрос не удался ({type(exc).__name__}); пробую по одному")
            full_tracks = []
            for key in missing:
                try:
                    full_tracks.append(client.tracks([key])[0])
                except Exception as inner:  # noqa: BLE001
                    eprint(f"  ! не удалось получить трек {key}: {type(inner).__name__}: {inner}")

        # Map by the same key format TrackShort uses: "<track id>:<album id>".
        for track in full_tracks:
            albums = getattr(track, "albums", None) or []
            album_id = getattr(albums[0], "id", None) if albums else None
            key = f"{track.id}:{album_id}" if album_id is not None else str(track.id)
            item = track_to_dict(track)
            item["metadata_source"] = "client_tracks"
            cache[key] = item

        # Fallback for anything the API omitted.
        for key in missing:
            if key in cache:
                continue
            short = next((s for s, k in zip(shorts, keys) if k == key), None)
            if short is None:
                continue
            embedded = getattr(short, "track", None)
            if embedded is not None:
                item = track_to_dict(embedded)
                item["metadata_source"] = "playlist_track"
                cache[key] = item
                continue
            try:
                item = track_to_dict(short.fetch_track())
                item["metadata_source"] = "fetch_track"
                cache[key] = item
            except Exception as exc:  # noqa: BLE001
                eprint(f"  ! не удалось получить трек {key}: {type(exc).__name__}: {exc}")
                cache[key] = short_to_dict(short)

    for key in keys:
        result.append(cache.get(key) or {"id": key, "error": "missing metadata"})
    return result


def playlist_to_dict(playlist: Any) -> dict[str, Any]:
    owner = getattr(playlist, "owner", None)
    owner_uid = getattr(owner, "uid", None) if owner is not None else getattr(playlist, "uid", None)
    return {
        "id": getattr(playlist, "playlist_uuid", None) or getattr(playlist, "id", None),
        "kind": getattr(playlist, "kind", None),
        "title": getattr(playlist, "title", None),
        "owner_uid": owner_uid,
        "track_count": getattr(playlist, "track_count", None),
        "visibility": getattr(playlist, "visibility", None),
        "description": getattr(playlist, "description", None),
        "created": getattr(playlist, "created", None),
        "modified": getattr(playlist, "modified", None),
        "tracks": [],
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Экспорт метаданных библиотеки Яндекс.Музыки")
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT, help="путь к JSON")
    parser.add_argument("--token-file", type=Path, default=TOKEN_PATH, help="файл с OAuth токеном")
    parser.add_argument("--login", action="store_true", help="принудительно выполнить Device Flow")
    parser.add_argument(
        "--per-track",
        action="store_true",
        help="использовать fetch_track() для каждого трека (медленно; по умолчанию пакетно)",
    )
    args = parser.parse_args()

    token = load_token(args.token_file, args.login)
    client = Client(token).init()
    if client.me is None or client.me.account is None:
        raise SystemExit("Не удалось авторизоваться: проверьте токен")

    account = client.me.account
    eprint(f"Аккаунт: {getattr(account, 'display_name', None) or getattr(account, 'login', None) or account.uid}")

    eprint("Получаю лайки...")
    likes_result = client.users_likes_tracks()
    like_shorts = list(likes_result.tracks) if likes_result is not None else []
    eprint(f"Лайков: {len(like_shorts)}")

    eprint("Получаю список плейлистов...")
    playlists = client.users_playlists_list() or []
    eprint(f"Плейлистов: {len(playlists)}")

    cache: dict[str, dict[str, Any]] = {}
    eprint("Загружаю полные метаданные лайков...")
    likes = resolve_tracks(client, like_shorts, per_track=args.per_track, cache=cache)

    playlist_items: list[dict[str, Any]] = []
    for index, playlist in enumerate(playlists, 1):
        item = playlist_to_dict(playlist)
        name = item["title"] or f"playlist #{index}"
        eprint(f"[{index}/{len(playlists)}] {name}")
        try:
            shorts = list(playlist.tracks or [])
            if not shorts and (playlist.track_count or 0) > 0:
                shorts = list(playlist.fetch_tracks())
            item["tracks"] = resolve_tracks(client, shorts, per_track=args.per_track, cache=cache)
            item["resolved_track_count"] = len(item["tracks"])
        except Exception as exc:  # noqa: BLE001 - one broken playlist must not abort the export
            eprint(f"  ! плейлист пропущен: {type(exc).__name__}: {exc}")
            item["error"] = f"{type(exc).__name__}: {exc}"
        playlist_items.append(item)

    playlist_track_total = sum(len(item.get("tracks", [])) for item in playlist_items)
    unique_ids = {track.get("id") for track in likes}
    for item in playlist_items:
        unique_ids.update(track.get("id") for track in item.get("tracks", []))
    unique_ids.discard(None)

    payload = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "account": {
            "uid": getattr(account, "uid", None),
            "login": getattr(account, "login", None),
            "display_name": getattr(account, "display_name", None),
        },
        "totals": {
            "likes": len(likes),
            "playlists": len(playlist_items),
            "playlist_tracks": playlist_track_total,
            "unique_track_ids": len(unique_ids),
        },
        "likes": likes,
        "playlists": playlist_items,
    }

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    eprint(f"JSON сохранён: {args.output}")
    print(
        json.dumps(
            {
                "output": str(args.output),
                "likes": len(likes),
                "playlists": len(playlist_items),
                "playlist_tracks": playlist_track_total,
                "unique_track_ids": len(unique_ids),
            },
            ensure_ascii=False,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
