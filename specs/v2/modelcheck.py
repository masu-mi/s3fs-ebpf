"""Tiny explicit-state model checker (BFS) used by the v2 spec models.

A model supplies:
  init            the initial state (hashable)
  step(s)         list of (label, next_state)
  invariants      list of (name, fn(state) -> bool); must hold in every reachable state
  is_final(s)     True for states where the scenario is allowed to stop
  final_checks    list of (name, fn(state) -> bool); must hold in every final state

explore() returns a Result.  A deadlock is a non-final state with no successor.
On a violation the shortest trace (BFS) from init is returned.
"""
from collections import deque
from dataclasses import dataclass, field


@dataclass
class Result:
    ok: bool
    states: int = 0
    kind: str = ""          # "invariant" | "final" | "deadlock" | "limit"
    name: str = ""
    trace: list = field(default_factory=list)   # [(label, state), ...]


def explore(init, step, invariants, is_final, final_checks=(), max_states=3_000_000):
    parent = {init: None}
    q = deque([init])

    def trace_to(s, last_label=None):
        out = []
        while s is not None:
            p = parent[s]
            out.append((p[1] if p else "init", s))
            s = p[0] if p else None
        out.reverse()
        return out

    while q:
        s = q.popleft()
        for name, fn in invariants:
            if not fn(s):
                return Result(False, len(parent), "invariant", name, trace_to(s))
        succ = step(s)
        if not succ:
            if not is_final(s):
                return Result(False, len(parent), "deadlock", "no enabled action", trace_to(s))
        if is_final(s):
            for name, fn in final_checks:
                if not fn(s):
                    return Result(False, len(parent), "final", name, trace_to(s))
        for label, t in succ:
            if t not in parent:
                parent[t] = (s, label)
                if len(parent) > max_states:
                    return Result(False, len(parent), "limit", "state limit")
                q.append(t)
    return Result(True, len(parent))


def show(res, fmt=repr, limit=40):
    if res.ok:
        return f"ok ({res.states} states)"
    lines = [f"VIOLATION {res.kind}: {res.name} ({res.states} states explored)"]
    for i, (lab, st) in enumerate(res.trace[-limit:]):
        lines.append(f"  {i:2d}. {lab:<28} {fmt(st)}")
    return "\n".join(lines)
