import asyncio
import io
import os
from datetime import timezone

from aiohttp import web
from hydrogram import Client
from hydrogram.errors import SessionPasswordNeeded


class MTProtoService:
    def __init__(self):
        self.api_id = int(os.getenv("API_ID", "2040"))
        self.api_hash = os.getenv("API_HASH", "b18441a1ff607e10a98964a5b8c7a4d0")
        self.phone = ""
        self.session_string = ""
        self.client = None
        self.authorized = False
        self.phone_code_hash = ""
        self.status = "disconnected"
        self.last_error = ""
        self.session_rejected = False
        self.lock = asyncio.Lock()

    async def configure(self, api_id, api_hash, phone="", session_string=""):
        async with self.lock:
            api_id = int(api_id or 2040)
            api_hash = api_hash or "b18441a1ff607e10a98964a5b8c7a4d0"
            if self.client and (api_id, api_hash, session_string) == (self.api_id, self.api_hash, self.session_string):
                self.phone = phone or self.phone
                if not session_string:
                    self.session_rejected = False
                return
            await self._stop()
            self.api_id, self.api_hash, self.phone = api_id, api_hash, phone or ""
            self.session_string = session_string or ""
            try:
                await self._create(self.session_string)
            except Exception as exc:
                # Do not destroy a persisted session because of a temporary
                # network/DNS/Telegram error. Logout/re-login remains available
                # explicitly through the admin UI.
                self.status = "error"
                self.last_error = str(exc)
                self.session_rejected = False
                raise
            else:
                self.session_rejected = False

    async def _create(self, session_string):
        self.status = "connecting"
        self.client = Client(
            "aggregator",
            api_id=self.api_id,
            api_hash=self.api_hash,
            session_string=session_string or None,
            in_memory=True,
            no_updates=True,
            workers=1,
            max_concurrent_transmissions=1,
        )
        await self.client.connect()
        try:
            await self.client.get_me()
            self.authorized, self.status = True, "authorized"
        except Exception:
            self.authorized, self.status = False, "connected"
        self.last_error = ""

    async def _stop(self):
        if self.client:
            try:
                await self.client.disconnect()
            except Exception:
                pass
        self.client, self.authorized = None, False

    async def ensure_client(self):
        if not self.client:
            await self.configure(self.api_id, self.api_hash, self.phone, self.session_string)
        return self.client

    async def exported_session(self):
        session = await self.client.export_session_string()
        self.session_string = session
        self.authorized, self.status = True, "authorized"
        return session


state = MTProtoService()


@web.middleware
async def errors(request, handler):
    try:
        return await handler(request)
    except web.HTTPException:
        raise
    except Exception as exc:
        state.last_error = str(exc)
        return web.json_response({"error": str(exc)}, status=503)


async def json_body(request):
    try:
        return await request.json()
    except Exception:
        raise web.HTTPBadRequest(text='{"error":"invalid JSON"}', content_type="application/json")


async def health(_):
    return web.json_response({"ok": True})


async def status(_):
    return web.json_response({"status": state.status, "authorized": state.authorized, "error": state.last_error, "session_rejected": state.session_rejected})


async def configure(request):
    body = await json_body(request)
    await state.configure(body.get("api_id"), body.get("api_hash"), body.get("phone", ""), body.get("session_string", ""))
    return await status(request)


async def send_code(request):
    body = await json_body(request)
    phone = str(body.get("phone", "")).strip()
    if not phone:
        raise web.HTTPBadRequest(text='{"error":"phone is required"}', content_type="application/json")
    client = await state.ensure_client()
    sent = await client.send_code(phone)
    state.phone, state.phone_code_hash, state.status = phone, sent.phone_code_hash, "code_sent"
    return await status(request)


async def sign_in(request):
    body = await json_body(request)
    if not state.phone_code_hash:
        raise web.HTTPBadRequest(text='{"error":"send code first"}', content_type="application/json")
    try:
        await state.client.sign_in(state.phone, state.phone_code_hash, str(body.get("code", "")).strip())
    except SessionPasswordNeeded:
        state.status = "password_required"
        return web.json_response({"status": state.status, "password_required": True, "session_string": ""})
    session = await state.exported_session()
    state.phone_code_hash = ""
    return web.json_response({"status": state.status, "password_required": False, "session_string": session})


async def check_password(request):
    body = await json_body(request)
    await state.client.check_password(str(body.get("password", "")))
    session = await state.exported_session()
    state.phone_code_hash = ""
    return web.json_response({"status": state.status, "password_required": False, "session_string": session})


async def logout(request):
    async with state.lock:
        if state.client and state.authorized:
            await state.client.log_out()
        await state._stop()
        state.session_string = ""
        state.session_rejected = False
        state.phone_code_hash = ""
        await state._create("")
    return await status(request)


def username(request):
    return request.match_info["username"].lstrip("@").lower()


async def resolve(request):
    if not state.authorized:
        raise web.HTTPUnauthorized(text='{"error":"Telegram Account is not authorized"}', content_type="application/json")
    name = username(request)
    chat = await state.client.get_chat("@" + name)
    kind_value = getattr(chat.type, "value", str(chat.type)).lower()
    kind = "mtproto_group" if "group" in kind_value else "web_channel"
    return web.json_response({"username": name, "name": chat.title or "@" + name, "kind": kind})


async def history(request):
    if not state.authorized:
        raise web.HTTPUnauthorized(text='{"error":"Telegram Account is not authorized"}', content_type="application/json")
    name = username(request)
    after_id = max(0, int(request.query.get("after_id", "0")))
    limit = min(500, max(1, int(request.query.get("limit", "500"))))
    chat = await state.client.get_chat("@" + name)
    messages, seen = [], set()
    async for message in state.client.get_chat_history("@" + name, limit=limit):
        if message.id <= after_id:
            break
        if message.id in seen:
            continue
        seen.add(message.id)
        published = message.date
        if published.tzinfo is None:
            published = published.replace(tzinfo=timezone.utc)
        messages.append({
            "message_id": message.id,
            "text": message.text or message.caption or "",
            "published_at": published.astimezone(timezone.utc).isoformat().replace("+00:00", "Z"),
            "original_url": f"https://t.me/{name}/{message.id}",
            "has_photo": message.photo is not None,
        })
    return web.json_response({"name": chat.title or "@" + name, "messages": messages})


async def photo(request):
    if not state.authorized:
        raise web.HTTPUnauthorized(text='{"error":"Telegram Account is not authorized"}', content_type="application/json")
    name, message_id = username(request), int(request.match_info["message_id"])
    message = await state.client.get_messages("@" + name, message_id)
    if not message or not message.photo:
        raise web.HTTPNotFound(text='{"error":"photo not found"}', content_type="application/json")
    data = await state.client.download_media(message.photo, in_memory=True)
    if data is None:
        raise web.HTTPNotFound(text='{"error":"photo download failed"}', content_type="application/json")
    payload = data.getvalue() if isinstance(data, io.BytesIO) else bytes(data.getbuffer())
    return web.Response(body=payload, content_type="image/jpeg")


app = web.Application(middlewares=[errors], client_max_size=64 * 1024)
app.add_routes([
    web.get("/health", health), web.get("/status", status), web.post("/configure", configure),
    web.post("/auth/send-code", send_code), web.post("/auth/sign-in", sign_in),
    web.post("/auth/check-password", check_password), web.post("/auth/logout", logout),
    web.get("/resolve/{username}", resolve), web.get("/history/{username}", history),
    web.get("/photo/{username}/{message_id}", photo),
])

if __name__ == "__main__":
    web.run_app(app, host="0.0.0.0", port=8081, access_log=None)
