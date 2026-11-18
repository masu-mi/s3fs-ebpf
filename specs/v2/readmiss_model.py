"""Read-miss protocol: readers, daemon handlers, an evictor, one inode, NCH chunks.

What it models (see docs/design/redesign-v2.md §6.1):
  reader   : check chunk -> pin -> join/create the in-flight record -> wait -> retry
  kmod     : record per chunk (generation), message queue, DONE (completion)
  daemon   : handlers that take a message, populate the chunk, send DONE
  evictor  : frees a populated chunk unless it is pinned (capacity pass / shed)

Knobs (each is a design decision or a historical bug):
  per_reader_enqueue  current code: every missing reader enqueues its own message
                      v2: only the creator of the in-flight record does
  done_by             "key": DONE completes whatever record is live for (ino, chunk)
                      "seq": DONE completes only the record the message belongs to
  verify              DONE(err=0) is refused (-EAGAIN) while the chunk is not populated
  pin                 reader pins its chunk before creating the record
  max_retries         scaled down from S3FS_UPCALL_MAX_RETRIES=3 to keep the space small
"""
from typing import NamedTuple
from modelcheck import explore, show


class Cfg(NamedTuple):
    readers: tuple = (0, 0, 0)     # target chunk of each reader
    nch: int = 1
    handlers: int = 2
    max_retries: int = 2
    evictions: int = 3
    timeouts: int = 0
    verify_tries: int = 2
    per_reader_enqueue: bool = True
    done_by: str = "key"
    verify: bool = True
    pin: bool = True


class S(NamedTuple):
    chunk: tuple    # 1 = populated
    pin: tuple
    rec: tuple      # generation of the live record, 0 = none
    gen: tuple      # last generation handed out
    done: tuple     # last generation completed
    msgs: tuple     # sorted multiset of (chunk, gen)
    rd: tuple       # per reader (pc, retries, gen, pinned)
    hd: tuple       # per handler (pc, chunk, gen, verify_tries)
    ev: int
    to: int


def _set(t, i, v):
    return t[:i] + (v,) + t[i + 1:]


def build(cfg: Cfg):
    n = cfg.nch
    init = S((0,) * n, (0,) * n, (0,) * n, (0,) * n, (0,) * n, (),
             tuple(("S", 0, 0, 0) for _ in cfg.readers),
             tuple(("IDLE", 0, 0, 0) for _ in range(cfg.handlers)), 0, 0)

    def finish(s, i, pc, pinned_now):
        c = cfg.readers[i]
        pin = s.pin
        if pinned_now:
            pin = _set(pin, c, pin[c] - 1)
        r = s.rd[i]
        return s._replace(pin=pin, rd=_set(s.rd, i, (pc, r[1], r[2], 0)))

    def step(s):
        out = []
        # ---- readers
        for i, (pc, retries, g, pinned) in enumerate(s.rd):
            c = cfg.readers[i]
            if pc == "S":
                if s.chunk[c]:
                    out.append((f"R{i} hit", finish(s, i, "OK", pinned)))
                elif cfg.pin and not pinned:
                    pin = _set(s.pin, c, s.pin[c] + 1)
                    out.append((f"R{i} pin c{c}", s._replace(pin=pin, rd=_set(s.rd, i, ("REQ", retries, g, 1)))))
                else:
                    out.append((f"R{i} ->req", s._replace(rd=_set(s.rd, i, ("REQ", retries, g, pinned)))))
            elif pc == "REQ":
                if retries >= cfg.max_retries:
                    out.append((f"R{i} EIO(retries)", finish(s, i, "EIO", pinned)))
                    continue
                msgs, rec, gen = s.msgs, s.rec, s.gen
                if rec[c] == 0:
                    gen = _set(gen, c, gen[c] + 1)
                    rec = _set(rec, c, gen[c])
                    msgs = tuple(sorted(msgs + ((c, gen[c]),)))
                    g2 = gen[c]
                else:
                    g2 = rec[c]
                    if cfg.per_reader_enqueue:
                        msgs = tuple(sorted(msgs + ((c, g2),)))
                out.append((f"R{i} req c{c} g{g2}",
                            s._replace(rec=rec, gen=gen, msgs=msgs, rd=_set(s.rd, i, ("WAIT", retries, g2, pinned)))))
            elif pc == "WAIT":
                if s.done[c] >= g:
                    out.append((f"R{i} wake", s._replace(rd=_set(s.rd, i, ("S", retries + 1, g, pinned)))))
                elif s.to < cfg.timeouts:
                    out.append((f"R{i} timeout", s._replace(to=s.to + 1, rd=_set(s.rd, i, ("S", retries + 1, g, pinned)))))
        # ---- daemon handlers
        for h, (pc, c, g, vt) in enumerate(s.hd):
            if pc == "IDLE":
                for m in sorted(set(s.msgs)):
                    msgs = list(s.msgs)
                    msgs.remove(m)
                    out.append((f"H{h} take c{m[0]} g{m[1]}",
                                s._replace(msgs=tuple(msgs), hd=_set(s.hd, h, ("GOT", m[0], m[1], 0)))))
            elif pc == "GOT":
                out.append((f"H{h} populate c{c}",
                            s._replace(chunk=_set(s.chunk, c, 1), hd=_set(s.hd, h, ("SIG", c, g, vt)))))
            elif pc == "SIG":
                if cfg.verify and not s.chunk[c] and vt < cfg.verify_tries:
                    out.append((f"H{h} EAGAIN->repop", s._replace(hd=_set(s.hd, h, ("GOT", c, g, vt + 1)))))
                    continue
                target = s.rec[c] if cfg.done_by == "key" else (g if s.rec[c] == g else 0)
                if target:
                    done = _set(s.done, c, max(s.done[c], target))
                    rec = _set(s.rec, c, 0)
                    out.append((f"H{h} DONE c{c} g{target}",
                                s._replace(done=done, rec=rec, hd=_set(s.hd, h, ("IDLE", 0, 0, 0)))))
                else:
                    out.append((f"H{h} DONE enoent", s._replace(hd=_set(s.hd, h, ("IDLE", 0, 0, 0)))))
        # ---- evictor (capacity pass / shed): never frees a pinned chunk
        if s.ev < cfg.evictions:
            for c in range(n):
                if s.chunk[c] and s.pin[c] == 0:
                    out.append((f"EVICT c{c}", s._replace(chunk=_set(s.chunk, c, 0), ev=s.ev + 1)))
        return out

    final_pcs = ("OK", "EIO")

    def is_final(s):
        return all(r[0] in final_pcs for r in s.rd)

    invs = []
    if cfg.timeouts == 0:
        invs.append(("no spurious EIO (daemon healthy)", lambda s: all(r[0] != "EIO" for r in s.rd)))
    finals = [("pins balanced", lambda s: all(p == 0 for p in s.pin))]
    return init, step, invs, is_final, finals


def run(cfg: Cfg):
    init, step, invs, is_final, finals = build(cfg)
    return explore(init, step, invs, is_final, finals)


def fmt(s):
    return (f"chunk={s.chunk} pin={s.pin} rec={s.rec} done={s.done} msgs={s.msgs} "
            f"rd={[r[0] + str(r[1]) for r in s.rd]} hd={[h[0] for h in s.hd]}")


if __name__ == "__main__":
    print(show(run(Cfg()), fmt))
