"""Run every v2 spec scenario and compare with the expected outcome.

  python3 docs/design/v2-spec/run.py            # table, exit 1 if any outcome differs
  python3 docs/design/v2-spec/run.py -v         # also print the counterexample traces

"expect ok"         the property holds in every reachable state of the scaled-down model
"expect VIOLATION"  the model must find a counterexample (a mutant / a known bug class);
                    if it stops finding one, the model has lost its teeth.
"""
import sys
import time

import flush_events_model as fe
import flush_model as fm
import readmiss_model as rm
from modelcheck import show

# Scaled stress for the read-miss model: 4 readers of one chunk, 3 handlers, 4 evictions,
# a retry budget of 1 (S3FS_UPCALL_MAX_RETRIES is 3 in the code; the shrunk budget keeps
# the space small and is what makes the C2 bug class reachable).
STRESS = dict(readers=(0, 0, 0, 0), handlers=3, max_retries=1, evictions=4)

V2 = dict(per_reader_enqueue=False, done_by="seq")

SCENARIOS = [
    # (id, module, config, expect_ok, meaning)
    ("RM-1", rm, rm.Cfg(**STRESS, verify_tries=4), True,
     "current protocol, verify never gives up"),
    ("RM-2", rm, rm.Cfg(**STRESS, verify_tries=2), False,
     "current protocol, verify gives up after 2 (signalVerifyRetries): a stale handler wakes a newer reader"),
    ("RM-3", rm, rm.Cfg(**STRESS, verify=False), False,
     "current protocol without verify: reproduces the AC-45 C2 bug class"),
    ("RM-4", rm, rm.Cfg(**STRESS, **V2), True,
     "v2: one message per in-flight record, DONE by seq, verify kept"),
    ("RM-5", rm, rm.Cfg(**STRESS, **V2, verify=False), True,
     "v2 without verify: VERIFY_POPULATED is no longer needed for correctness"),
    ("RM-6", rm, rm.Cfg(**STRESS, per_reader_enqueue=False, done_by="key", verify=False), True,
     "only one-message-per-record (DONE by key) is already enough here"),
    ("RM-7", rm, rm.Cfg(**STRESS, per_reader_enqueue=True, done_by="seq", verify=False), True,
     "only DONE-by-seq (one message per reader) is already enough here"),
    ("RM-8", rm, rm.Cfg(**STRESS, **V2, pin=False), False,
     "v2 without the pre-request chunk pin: reader loses its chunk"),
    ("RM-9", rm, rm.Cfg(readers=(0, 0, 1, 1), nch=2, handlers=3, max_retries=1, evictions=4, **V2, verify=False), True,
     "v2, two chunks, readers on both"),
    ("RM-10", rm, rm.Cfg(readers=(0, 0, 0), handlers=2, max_retries=2, evictions=3, timeouts=2, **V2), True,
     "v2 with daemon timeouts: no deadlock, pins balanced (EIO allowed, so only safety is checked)"),
    ("FL-1", fm, fm.Cfg(), True,
     "v2 flush slot: close/fsync/write/PUT-failure interleavings"),
    ("FL-2", fm, fm.Cfg(writes=3, closes=3, fsyncs=2, fails=2), True,
     "v2 flush slot, larger bounds"),
    ("FL-3", fm, fm.Cfg(check_seq=False), False,
     "no flush_read_seq check on DONE: a write after the PUT's read is lost (W3 class)"),
    ("FL-4", fm, fm.Cfg(requeue=False), False,
     "slot merged but no re-queue on seq mismatch: an fsync waiter sleeps forever / write lost"),
    # The message flow that is implemented (AC-94): one message per enqueue, DONE(ino, err).
    ("FE-1", fe, fe.Cfg(), True,
     "one FLUSH message per enqueue, unified DONE(ino, err): close/fsync/write/PUT-failure interleavings"),
    ("FE-2", fe, fe.Cfg(writes=3, closes=3, fsyncs=2, fails=2), True,
     "same, larger bounds"),
    ("FE-3", fe, fe.Cfg(drop_on_start=False), True,
     "the daemon does not collapse messages (one PUT per message): also safe"),
    ("FE-4", fe, fe.Cfg(check_seq=False), False,
     "no flush_read_seq check on DONE ok: a write after the PUT's read is lost (W3)"),
    ("FE-5", fe, fe.Cfg(queue_when_clean=False), False,
     "fsync of a clean file with a flush in flight queues no message: a DONE that does not cover leaves it asleep (model finding)"),
    ("FE-6", fe, fe.Cfg(fsync_waits_flushing=False), False,
     "fsync of a clean file returns at once while its flush is in flight (the code before AC-96): not durable"),
    ("FE-7", fe, fe.Cfg(split_done=True), False,
     "old protocol, MARK_DIRTY and SIGNAL_FLUSH(err) as two steps: a close in between leaves flushing=0 dirty=0 with a message queued (AC-95); one DONE(err) closes the window"),
]


def main(argv):
    verbose = "-v" in argv
    bad = 0
    print(f"{'id':6} {'expect':10} {'result':10} {'states':>8}  meaning")
    for sid, mod, cfg, expect_ok, meaning in SCENARIOS:
        t0 = time.time()
        res = mod.run(cfg)
        flag = "" if res.ok == expect_ok else "   <-- MISMATCH"
        bad += res.ok != expect_ok
        print(f"{sid:6} {'ok' if expect_ok else 'VIOLATION':10} {'ok' if res.ok else 'VIOLATION':10} "
              f"{res.states:8d}  {meaning}{flag}")
        if verbose and not res.ok:
            print(f"       {res.kind}: {res.name}")
            print(show(res, mod.fmt, limit=30))
    print("\nall outcomes as expected" if not bad else f"\n{bad} scenario(s) differ from the expectation")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
