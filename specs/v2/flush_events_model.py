"""Flush protocol with one message per enqueue and a unified DONE (AC-94, Phase 4 step 2).

This is the protocol the code implements: every close / fsync that finds the file dirty
queues its own FLUSH message (no per-inode slot; a slot would carry a stale name, see
docs/design/redesign-v2-simplify.md 4.3), the daemon's debounce collapses the messages of
one inode into one PUT, and the completion of the PUT is a single DONE(ino, err) that

    ok  : covers the flush only if flush_read_seq == flush_seq (W3), then clears flushing,
          unpins when clean, wakes the fsync waiters
    err : sets dirty again and clears flushing (this was a separate MARK_DIRTY ioctl) and
          wakes the waiters with the error; the pin stays

The slot model (flush_model.py) is the verified shape of the *kmod state*; this model
checks the shape of the *message flow* that is kept: messages are not 1:1 with PUTs.

Actors: writer, close(2) (enqueue), fsync(2) (enqueue + wait), daemon (start = take the
queued messages + READ_DIRTY, PUT, DONE).  The daemon handles one inode's flush at a time
(flush_order.go).

Knobs (each is a design decision or a historical bug):
  check_seq            DONE ok is covered only if flush_read_seq == flush_seq       (W3)
  fsync_waits_flushing an fsync of a clean file whose flush is still in flight waits for
                       it (current code returns 0 at once: model finding, AC-95)
  split_done           PUT failure = MARK_DIRTY then SIGNAL(err) as two steps (the old
                       protocol) instead of one DONE(err)
  queue_when_clean     that fsync (clean file, flush in flight) queues its own message; without
                       it a DONE that does not cover leaves the fsync asleep forever (model finding)
  drop_on_start        the daemon's start drops the messages queued so far (they are
                       covered by the read); off = every message makes its own PUT
"""
from typing import NamedTuple
from modelcheck import explore, show


class Cfg(NamedTuple):
    writes: int = 2
    closes: int = 2
    fsyncs: int = 1
    fails: int = 1
    check_seq: bool = True
    fsync_waits_flushing: bool = True
    split_done: bool = False
    drop_on_start: bool = True
    queue_when_clean: bool = True


class S(NamedTuple):
    cv: int          # content version in the kmod buffer
    nw: int
    nc: int
    nf: int
    nfail: int
    dirty: bool
    flushing: bool
    pinned: bool
    fseq: int        # fd->flush_seq: bumped by every enqueue
    rseq: int        # fd->flush_read_seq: fseq when READ_DIRTY last ran
    msgs: int        # FLUSH messages queued (kmod FIFO + daemon debounce)
    ph: str          # daemon: IDLE | READ | PUTOK | PUTFAIL | MARKED
    rv: int          # version the daemon read
    ov: int          # version in the object store
    fs: tuple        # (state, need): state in none|wait|ok|err


def build(cfg: Cfg):
    init = S(0, 0, 0, 0, 0, False, False, False, 0, 0, 0, "IDLE", 0, 0, ("none", 0))

    def enqueue(s):
        return s._replace(fseq=s.fseq + 1, dirty=False, flushing=True, pinned=True, msgs=s.msgs + 1)

    def wake(s, state):
        return ("ok" if state == "ok" else "err", s.fs[1]) if s.fs[0] == "wait" else s.fs

    def done_ok(s):
        t = s
        covered = (s.rseq == s.fseq) or not cfg.check_seq
        if covered:
            t = t._replace(flushing=False, pinned=s.pinned and s.dirty, fs=wake(s, "ok"))
        return t._replace(ph="IDLE")

    def step(s):
        out = []
        if s.nw < cfg.writes:
            out.append(("write", s._replace(cv=s.cv + 1, nw=s.nw + 1, dirty=True)))
        if s.nc < cfg.closes and s.dirty:
            out.append(("close", enqueue(s)._replace(nc=s.nc + 1)))
        if s.nf < cfg.fsyncs and s.fs[0] == "none":
            if s.dirty:
                out.append(("fsync", enqueue(s)._replace(nf=s.nf + 1, fs=("wait", s.cv))))
            elif s.flushing and cfg.fsync_waits_flushing:
                # clean, but a flush is in flight: wait for the one that covers cv
                # it must queue its own message: the in-flight PUT may predate cv, and
                # a DONE that does not cover (rseq != fseq) re-flushes nothing by itself
                t = s._replace(nf=s.nf + 1, fs=("wait", s.cv))
                out.append(("fsync (flush in flight)",
                            t._replace(fseq=s.fseq + 1, msgs=s.msgs + 1) if cfg.queue_when_clean
                            else t._replace(fseq=s.fseq + 1)))
            else:
                out.append(("fsync (clean)", s._replace(nf=s.nf + 1, fs=("ok", s.cv))))
        if s.ph == "IDLE" and s.msgs > 0:
            m = 0 if cfg.drop_on_start else s.msgs - 1
            out.append(("daemon start, READ_DIRTY", s._replace(ph="READ", rv=s.cv, rseq=s.fseq, msgs=m)))
        if s.ph == "READ":
            out.append(("PUT ok", s._replace(ph="PUTOK", ov=max(s.ov, s.rv))))
            if s.nfail < cfg.fails:
                out.append(("PUT fail", s._replace(ph="PUTFAIL", nfail=s.nfail + 1)))
        if s.ph == "PUTOK":
            out.append(("DONE ok", done_ok(s)))
        if s.ph == "PUTFAIL":
            if cfg.split_done:
                out.append(("MARK_DIRTY", s._replace(ph="MARKED", dirty=True)))
            else:
                out.append(("DONE err", s._replace(ph="IDLE", dirty=True, flushing=False, fs=wake(s, "err"))))
        if s.ph == "MARKED":
            out.append(("SIGNAL err", s._replace(ph="IDLE", flushing=False, fs=wake(s, "err"))))
        return out

    invs = [
        ("no lost write: object stale => dirty or flushing", lambda s: s.ov >= s.cv or s.dirty or s.flushing),
        ("flushing => inode pinned", lambda s: not s.flushing or s.pinned),
        ("flushing => a request exists or the daemon is on it",
         lambda s: not s.flushing or s.msgs > 0 or s.ph != "IDLE"),
        ("fsync waiting => flush in progress", lambda s: s.fs[0] != "wait" or s.flushing or s.ph != "IDLE"),
        ("fsync ok => object has the data it asked for", lambda s: s.fs[0] != "ok" or s.ov >= s.fs[1]),
    ]
    is_final = lambda s: s.ph == "IDLE" and s.msgs == 0 and s.fs[0] != "wait"
    return init, step, invs, is_final, []


def run(cfg: Cfg):
    init, step, invs, is_final, finals = build(cfg)
    return explore(init, step, invs, is_final, finals)


def fmt(s):
    return (f"cv={s.cv} ov={s.ov} dirty={int(s.dirty)} flushing={int(s.flushing)} pin={int(s.pinned)} "
            f"seq={s.fseq}/{s.rseq} msgs={s.msgs} daemon={s.ph} fsync={s.fs}")


if __name__ == "__main__":
    print(show(run(Cfg()), fmt))
