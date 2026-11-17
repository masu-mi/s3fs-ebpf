"""Flush protocol of v2: one flush slot per inode (docs/design/redesign-v2.md §6.2).

Actors: a writer, close(2) (non-blocking enqueue), fsync(2) (enqueue + wait),
the daemon (take, read the dirty buffer, PUT, DONE ok/err).

kmod state per inode: dirty, flushing, pinned (inode held until the PUT), flush_seq,
flush_read_seq, slot in {IDLE, QUEUED, TAKEN, READ}.  The slot replaces today's
flush queue + flush_wait queue + flush_pending hash + F1 "drop queued items" rule:
a second enqueue while the slot is busy only bumps flush_seq.

Knobs reproduce known bug classes (W3, lost update):
  check_seq  DONE(ok) wakes waiters / clears flushing only if flush_read_seq == flush_seq
  requeue    on a seq mismatch the kmod re-queues the slot (needed because enqueues merge)
"""
from typing import NamedTuple
from modelcheck import explore, show


class Cfg(NamedTuple):
    writes: int = 2
    closes: int = 2
    fsyncs: int = 1
    fails: int = 1
    check_seq: bool = True
    requeue: bool = True


class S(NamedTuple):
    cv: int          # content version in the kmod buffer
    nw: int
    nc: int
    nf: int
    nfail: int
    dirty: bool
    flushing: bool
    pinned: bool
    fseq: int
    rseq: int
    slot: str        # IDLE | QUEUED | TAKEN | READ
    rv: int          # version the daemon read
    ov: int          # version in the object store
    fs: tuple        # (state, need): state in none|wait|ok|err


def build(cfg: Cfg):
    init = S(0, 0, 0, 0, 0, False, False, False, 0, 0, "IDLE", 0, 0, ("none", 0))

    def enqueue(s):
        slot = "QUEUED" if s.slot == "IDLE" else s.slot
        return s._replace(fseq=s.fseq + 1, dirty=False, flushing=True, pinned=True, slot=slot)

    def step(s):
        out = []
        if s.nw < cfg.writes:
            out.append(("write", s._replace(cv=s.cv + 1, nw=s.nw + 1, dirty=True)))
        if s.nc < cfg.closes and s.dirty:
            out.append(("close", enqueue(s)._replace(nc=s.nc + 1)))
        if s.nf < cfg.fsyncs and s.fs[0] == "none":
            if s.dirty or s.flushing:
                t = s._replace(nf=s.nf + 1, fs=("wait", s.cv))
                out.append(("fsync", enqueue(t) if s.dirty else t._replace(fseq=t.fseq + 1)))
            else:
                out.append(("fsync (clean)", s._replace(nf=s.nf + 1, fs=("ok", s.cv))))
        if s.slot == "QUEUED":
            out.append(("daemon take", s._replace(slot="TAKEN")))
        if s.slot == "TAKEN":
            out.append(("daemon READ_DIRTY", s._replace(slot="READ", rv=s.cv, rseq=s.fseq)))
        if s.slot == "READ":
            ov = max(s.ov, s.rv)
            t = s._replace(ov=ov)
            match = (s.rseq == s.fseq) or not cfg.check_seq
            if match:
                fs = ("ok", s.fs[1]) if s.fs[0] == "wait" else s.fs
                out.append(("PUT ok, DONE", t._replace(slot="IDLE", flushing=False,
                                                       pinned=s.pinned and s.dirty, fs=fs)))
            else:
                out.append(("PUT ok, DONE (seq mismatch)",
                            t._replace(slot="QUEUED" if cfg.requeue else "IDLE",
                                       flushing=cfg.requeue)))
            if s.nfail < cfg.fails:
                fs = ("err", s.fs[1]) if s.fs[0] == "wait" else s.fs
                out.append(("PUT fail, DONE(err)", s._replace(nfail=s.nfail + 1, slot="IDLE", flushing=False,
                                                              dirty=True, fs=fs)))
        return out

    invs = [
        ("no lost write: object stale => dirty or flushing", lambda s: s.ov >= s.cv or s.dirty or s.flushing),
        ("flushing => inode pinned", lambda s: not s.flushing or s.pinned),
        ("slot busy => flushing", lambda s: s.slot == "IDLE" or s.flushing),
        ("flushing => a request exists", lambda s: not s.flushing or s.slot != "IDLE"),
        ("fsync waiting => flush in progress", lambda s: s.fs[0] != "wait" or s.flushing),
        ("fsync ok => object has the data it asked for", lambda s: s.fs[0] != "ok" or s.ov >= s.fs[1]),
    ]
    is_final = lambda s: s.slot == "IDLE" and s.fs[0] != "wait"
    return init, step, invs, is_final, []


def run(cfg: Cfg):
    init, step, invs, is_final, finals = build(cfg)
    return explore(init, step, invs, is_final, finals)


def fmt(s):
    return (f"cv={s.cv} ov={s.ov} dirty={int(s.dirty)} flushing={int(s.flushing)} pin={int(s.pinned)} "
            f"seq={s.fseq}/{s.rseq} slot={s.slot} fsync={s.fs}")


if __name__ == "__main__":
    print(show(run(Cfg()), fmt))
