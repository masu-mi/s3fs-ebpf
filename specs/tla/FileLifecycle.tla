------------------------------ MODULE FileLifecycle ------------------------------
(*
 * Lifecycle of a file across kmod, the daemon and S3 (create, rename, flush,
 * sync, open/read). See docs/design/file-lifecycle-state-machine.md for the
 * state diagram and the mapping from each action to the code.
 *
 * What it checks:
 *   OpenOK / ReadOK       a file the user can see, whose bytes exist in S3
 *                         under SOME key, must be found by the daemon under the
 *                         key it resolves (open_check's HEAD, a read miss's GET)
 *   QuiescentConsistent   when the daemon has nothing left to do, S3 holds each
 *                         file under the name the user gave it
 *
 * Variants (constants):
 *   RecordKeys     the daemon's flush / rename also update its entry mirror
 *                  (backlog AC-82 fix). FALSE = the code as of PR #44.
 *   Serialized     the daemon handles flush and rename events in the order
 *                  kmod queued them. FALSE = today: the flush loop and the
 *                  rename loop are independent tickers, so a later event of
 *                  one kind can overtake an earlier event of the other.
 *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS Names, Inodes, NONE, RecordKeys, Serialized, MaxQ

VARIABLES
    kd,        \* kmod entry table: name -> ino (what the user sees)
    s3,        \* S3 bucket: key -> ino whose bytes it holds
    mirror,    \* daemon entry mirror (fpebpf): ino -> S3 key, learned at sync
    local,     \* daemon localKeys: ino -> key the daemon last wrote/renamed
    used,      \* inodes ever allocated
    evQ        \* kmod -> daemon events, in the order kmod queued them

vars == <<kd, s3, mirror, local, used, evQ>>

TypeOK ==
    /\ kd \in [Names -> Inodes \cup {NONE}]
    /\ s3 \in [Names -> Inodes \cup {NONE}]
    /\ mirror \in [Inodes -> Names \cup {NONE}]
    /\ local \in [Inodes -> Names \cup {NONE}]

\* Ino 1 exists at "a" in S3, in kmod and in the daemon's mirror (a synced file).
Init ==
    /\ kd     = [n \in Names |-> IF n = "a" THEN 1 ELSE NONE]
    /\ s3     = [n \in Names |-> IF n = "a" THEN 1 ELSE NONE]
    /\ mirror = [i \in Inodes |-> IF i = 1 THEN "a" ELSE NONE]
    /\ local  = [i \in Inodes |-> NONE]
    /\ used   = {1}
    /\ evQ = <<>>

\* resolveEventS3Key: the mirror's key if it knows the ino, else the event's name.
EvKey(i, nm) == IF mirror[i] # NONE THEN mirror[i] ELSE nm

\* open_check (handleOpenCheck): localKeys first, then the mirror.
OpenKey(i) == IF local[i] # NONE THEN local[i] ELSE mirror[i]
\* read miss (handleReadMiss): the mirror only.
ReadKey(i) == mirror[i]

Has(k, i) == k \in Names /\ s3[k] = i

\* -- kmod side ----------------------------------------------------------------
Create(n, i) ==
    /\ Len(evQ) < MaxQ   \* bound for the model checker (queued events)
    /\ kd[n] = NONE /\ i \in Inodes \ used
    /\ kd' = [kd EXCEPT ![n] = i]
    /\ used' = used \cup {i}
    /\ evQ' = Append(evQ, [type |-> "flush", ino |-> i, name |-> n])
    /\ UNCHANGED <<s3, mirror, local>>

Rename(f, t) ==
    /\ Len(evQ) < MaxQ
    /\ kd[f] # NONE /\ kd[t] = NONE
    /\ kd' = [kd EXCEPT ![t] = kd[f], ![f] = NONE]
    /\ evQ' = Append(evQ, [type |-> "rename", ino |-> kd[f], from |-> f, to |-> t])
    /\ UNCHANGED <<s3, mirror, local, used>>

\* -- daemon side (independent loops) -------------------------------------------
\* Event j is the oldest of its kind (each loop is FIFO); Serialized also
\* requires it to be the oldest event overall.
Oldest(j, t) ==
    /\ evQ[j].type = t
    /\ \A m \in 1..(j-1): evQ[m].type # t
    /\ (Serialized => j = 1)

Without(j) == SubSeq(evQ, 1, j-1) \o SubSeq(evQ, j+1, Len(evQ))

DaemonFlush ==
    \E j \in 1..Len(evQ):
        /\ Oldest(j, "flush")
        /\ LET ev == evQ[j]
               k  == EvKey(ev.ino, ev.name)
           IN /\ s3' = [s3 EXCEPT ![k] = ev.ino]
              /\ local' = [local EXCEPT ![ev.ino] = k]
              /\ mirror' = IF RecordKeys THEN [mirror EXCEPT ![ev.ino] = k] ELSE mirror
        /\ evQ' = Without(j)
        /\ UNCHANGED <<kd, used>>

DaemonRename ==
    \E j \in 1..Len(evQ):
        /\ Oldest(j, "rename")
        /\ LET ev  == evQ[j]
               old == EvKey(ev.ino, ev.from)
           IN IF s3[old] # NONE /\ old # ev.to
                \* s3c.Rename = copy + delete; a missing source is an error, dropped
                THEN /\ s3' = [s3 EXCEPT ![ev.to] = s3[old], ![old] = NONE]
                     /\ local' = [local EXCEPT ![ev.ino] = ev.to]
                     /\ mirror' = IF RecordKeys THEN [mirror EXCEPT ![ev.ino] = ev.to] ELSE mirror
                ELSE UNCHANGED <<s3, local, mirror>>
        /\ evQ' = Without(j)
        /\ UNCHANGED <<kd, used>>

\* syncEntriesOnce: the mirror is rebuilt from the S3 listing.
Sync ==
    /\ mirror' = [i \in Inodes |->
                    IF \E n \in Names: s3[n] = i THEN CHOOSE n \in Names: s3[n] = i
                    ELSE mirror[i]]
    /\ UNCHANGED <<kd, s3, local, used, evQ>>

Next ==
    \/ \E n \in Names, i \in Inodes: Create(n, i)
    \/ \E f, t \in Names: Rename(f, t)
    \/ DaemonFlush \/ DaemonRename \/ Sync

Spec == Init /\ [][Next]_vars

\* -- properties ----------------------------------------------------------------
OpenOK ==
    \A n \in Names:
        (kd[n] # NONE /\ \E k \in Names: s3[k] = kd[n]) => Has(OpenKey(kd[n]), kd[n])

ReadOK ==
    \A n \in Names:
        (kd[n] # NONE /\ \E k \in Names: s3[k] = kd[n]) => Has(ReadKey(kd[n]), kd[n])

QuiescentConsistent ==
    (evQ = <<>>) =>
        \A n \in Names: kd[n] # NONE => s3[n] = kd[n]
==================================================================================
