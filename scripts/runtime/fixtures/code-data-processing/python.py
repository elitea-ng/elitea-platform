import asyncio
import base64
import csv
import gzip
import io
import json
import time
from dataclasses import dataclass
import micropip

started = time.perf_counter()
await micropip.install(['python-slugify==8.0.4', 'python-dateutil==2.9.0.post0'])
from slugify import slugify
from dateutil.parser import isoparse

@dataclass(frozen=True, slots=True)
class Transaction:
    identifier: int
    category: int
    cents: int
    status: int

    @property
    def signed_cents(self):
        return self.cents * self.status

class TransactionFactory:
    def __init__(self, seed):
        self.seed = seed
        self.merchants = [slugify(name) for name in ['Café North', 'Déjà Vu', 'Office & Co', 'Travel Hub', 'Books Plus']]
        self.days = [isoparse(f'2026-09-{day:02d}T12:00:00+00:00').date().isoformat() for day in range(1, 29)]

    async def batch(self, first, last):
        await asyncio.sleep(0)
        return [Transaction(i, (i * 7 + self.seed) % 5, (i * 7919 + self.seed) % 100000 + 100,
                            0 if i % 19 == 0 else (-1 if i % 23 == 0 else 1)) for i in range(first, last)]

config = json.loads(elitea_state['input'])
count, seed = int(config['rows']), int(config['seed'])
assert 1 <= count <= 20000
factory = TransactionFactory(seed)
batches = await asyncio.gather(*(factory.batch(i, min(i + 500, count)) for i in range(0, count, 500)))
rows = sorted((row for batch in batches for row in batch), key=lambda row: (row.cents, row.identifier))
buffer = io.StringIO(newline='')
writer = csv.writer(buffer, lineterminator='\n')
writer.writerow(['id', 'category', 'cents', 'status'])
writer.writerows((row.identifier, row.category, row.cents, row.status) for row in rows)
raw = buffer.getvalue().encode()
compressed = gzip.compress(raw, compresslevel=6, mtime=0)
{'payload': base64.b64encode(compressed).decode(), 'rows': count, 'seed': seed,
 'merchants': factory.merchants, 'first_day': factory.days[0], 'last_day': factory.days[-1],
 'metrics': [{'stage': 'python', 'processing_ms': round((time.perf_counter() - started) * 1000, 3),
              'raw_bytes': len(raw), 'compressed_bytes': len(compressed)}]}
