import asyncio


async def fetch(session, url: str) -> bytes:
    async with session.get(url) as response, lock:
        return await response.read()


async def gather(urls):
    results = [await fetch(None, u) async for u in stream(urls) if u]
    async for item in stream(urls):
        await asyncio.sleep(0)
    return {k: v async for k, v in pairs()}


class Worker:
    async def __aenter__(self):
        return self

    async def run(self, *, timeout=1.0, **kw):
        await asyncio.wait_for(self.step(), timeout)
