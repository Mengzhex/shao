# Buffer sizing

The buffer only has to be big enough to still hold what you are about to ask
about. That is the whole question, and it has a measurable answer rather than a
guessed one.

## How the buffer behaves

Each session's stream is written as fixed-size segment files. When the total
exceeds the cap, the **oldest whole segment** is deleted. Two consequences
follow, and both matter:

- What remains is always **one unbroken run** of the stream. Old output falls
  off the back; nothing is ever removed from the middle. "The last N lines" is
  never stitched together across a hole.
- Loss is **visible**. Every segment records the absolute stream offset of its
  first byte, so a reader verifies contiguity rather than assuming it. Reads
  report `truncated` (the ring dropped older output — expected) separately from
  `gap_detected` (a genuine discontinuity — should never happen, and is
  surfaced loudly if it does).

## The numbers

| Setting | Default | What it covers |
|---|---|---|
| `buffer.max_bytes` | 256 MiB | raw bytes per session |
| `buffer.cooked_max_bytes` | 64 MiB | readable line stream per session |
| `buffer.segment_bytes` | 4 MiB | eviction granularity |
| `buffer.global_disk_cap` | 4 GiB | all sessions together |
| `buffer.retain_days` | 7 | finished sessions are deleted after this |

At the 2–5 MB/min that heavy log spam produces, 256 MiB reaches back roughly
one to two hours of *continuous* scrolling. Ordinary interactive work produces
far less, so the same buffer covers days.

The cooked stream is smaller on purpose: it is what the AI reads, and stripping
escape sequences and collapsing redraws typically shrinks output several-fold.

## Sizing it against your actual load

Do not estimate. Measure:

```
$ tmon sessions
20260827T142530.412-8134  deploy  live  web-prod-1  118.4MB buffered  3.2 MB/min  ~1.3h back  47 cmds
```

`3.2 MB/min` is that session's measured rate and `~1.3h back` is how far its
buffer currently reaches at that rate. If the gap between when something breaks
and when you get around to asking is longer than that number, raise the cap.

## Raising it

Globally, in `~/.tmon/config.yaml`:

```yaml
buffer:
  max_bytes: 1073741824        # 1 GiB
  cooked_max_bytes: 268435456  # 256 MiB
  global_disk_cap: 17179869184 # 16 GiB — raise this too, or it is the real limit
```

Or for one noisy session only, which is usually the better answer — one build
log does not justify a bigger buffer for every shell you open:

```yaml
sessions:
  - label: deploy
    host: web-prod-1
    max_bytes: 2147483648      # 2 GiB, applied to `tmon shell --label deploy`
```

Two constraints worth knowing:

- A ring is forced to at least two segments. A cap below `2 × segment_bytes` is
  raised automatically, because evicting down to a single segment would target
  the one being written.
- `global_disk_cap` is enforced when a new session starts, by deleting whole
  **ended** sessions oldest-first. Live sessions are never touched.
- `retain_days` bounds history by age as well as size, enforced at the same
  moment. A size cap alone means sessions accumulate indefinitely on a quiet
  machine and are only reclaimed when the total happens to hit the limit. Set
  it negative to keep everything.

## Freshness: what "recent" means

The reader and the recorder are different processes, so there is a small window
between a byte being written and a reader being able to see it. Two rules bound
it:

| Setting | Default | Effect |
|---|---|---|
| `buffer.flush_interval_ms` | 200 | during continuous output, a reader is at most this far behind |
| `buffer.idle_flush_ms` | 50 | once output stops, everything is on disk within this |

The second is the one that matters in practice. The moment a command finishes
and you turn to ask what went wrong, output has stopped, so the buffer is
already complete. Lowering these costs more small writes and buys almost
nothing.

## Segment size

`segment_bytes` trades eviction granularity against file count. At 4 MiB, a
256 MiB ring is 64 files and eviction discards 4 MiB at a time. Making it much
smaller means thousands of files per session; much larger means the ring
overshoots its cap by more between evictions. The default is fine unless you
have a specific reason.

One Windows-specific note: a segment cannot be deleted while a reader in
another process has it open, so eviction can briefly overshoot the cap and
retries on the next rollover. Overshooting for a few seconds is much cheaper
than the alternative — skipping ahead to delete a newer segment would punch a
real hole in the stream.
