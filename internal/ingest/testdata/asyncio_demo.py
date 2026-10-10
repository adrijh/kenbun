"""asyncio app for kenbun's LOOP view (testdata/asyncio.jsonl.gz was captured from
it). Every second one handler blocks the loop in time.sleep (off CPU), one
blocks it computing (on CPU) and one awaits properly. Runs until killed.

Every 5s it prints how long each handler really held the loop, to compare
with what kenbun shows.
"""
import asyncio
import contextlib
import time

held = {}  # handler -> seconds it held the loop


def fib(n):
    return n if n < 2 else fib(n - 1) + fib(n - 2)


# A with block, not a decorator: a wrapper would be the frame kenbun names as
# the callback for every handler.
@contextlib.contextmanager
def timed(name):
    t = time.perf_counter()
    yield
    held[name] = held.get(name, 0) + time.perf_counter() - t


async def sleepy_handler():
    with timed("sleepy_handler"):
        time.sleep(0.2)  # blocks the loop, waiting


async def cpu_handler():
    with timed("cpu_handler"):
        fib(30)  # blocks the loop on CPU, ~0.1s


async def good_handler():
    await asyncio.sleep(0.2)  # yields: the loop is idle meanwhile


async def main():
    start = time.perf_counter()
    while True:
        for h in (sleepy_handler, cpu_handler, good_handler):
            asyncio.create_task(h())
        await asyncio.sleep(1)
        if (elapsed := time.perf_counter() - start) >= 5:
            parts = "  ".join(f"{k} {100 * v / elapsed:.1f}%" for k, v in held.items())
            print(f"loop blocked {100 * sum(held.values()) / elapsed:.1f}%  {parts}", flush=True)
            held.clear()
            start = time.perf_counter()


asyncio.run(main())
