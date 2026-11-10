----------------------------- MODULE ObjStaging ------------------------------
(*
 * The daemon's small-object staging (AC-88): while a reader misses on file i of a
 * directory, the daemon already GETs the next files into memory, so the miss of
 * file i+1 populates from memory instead of waiting for a GET.  See
 * docs/design/object-staging.md.
 *
 * One object key.  The model keeps the facts that decide whether a reader can be
 * served an old copy:
 *   s3       the version S3 holds (a local write makes version s3+1 after its PUT);
 *   flushing a write's PUT is in flight (the file is dirty: no clean miss);
 *   chunk    what kmod holds for the file after a miss (NONE = nothing);
 *   stage    the staged copy (NONE or a version);
 *   gen      a counter the daemon bumps when it invalidates the key;
 *   infl     one prefetch in flight: the gen at issue, and the version S3 served
 *            (NONE until the GET has been served).
 * Every change of the object goes through a local write (the daemon's flush); an
 * external writer is covered by the staging TTL, not by this model.
 *
 * Property Fresh: after a clean miss, kmod holds what S3 holds.
 *
 * Variants:
 *   InvalStart  the write drops the staged copy and bumps gen when the PUT starts;
 *   InvalDone   ... when the PUT completes (the daemon's flush loop learns it);
 *   GenCheck    a finished prefetch stores its copy only if gen is unchanged since
 *               it was issued.
 * The design is InvalDone /\ GenCheck (InvalStart as well is harmless).
 *)
EXTENDS Naturals

CONSTANTS NONE, MaxVer, InvalStart, InvalDone, GenCheck

VARIABLES s3, flushing, chunk, stage, gen, infl

vars == <<s3, flushing, chunk, stage, gen, infl>>

Vers == 0..(MaxVer + 1)
Infl == {NONE} \cup [g : 0..(2 * MaxVer + 1), v : Vers \cup {NONE}]

TypeOK == /\ s3 \in 0..MaxVer
          /\ flushing \in BOOLEAN
          /\ chunk \in Vers \cup {NONE}
          /\ stage \in Vers \cup {NONE}
          /\ gen \in 0..(2 * MaxVer + 1)
          /\ infl \in Infl

Init == /\ s3 = 0 /\ flushing = FALSE /\ chunk = NONE
        /\ stage = NONE /\ gen = 0 /\ infl = NONE

(* A local write: the file holds the new bytes and its PUT starts. *)
Write == /\ ~flushing /\ s3 < MaxVer
         /\ flushing' = TRUE
         /\ chunk' = s3 + 1
         /\ IF InvalStart THEN /\ stage' = NONE /\ gen' = gen + 1
                          ELSE UNCHANGED <<stage, gen>>
         /\ UNCHANGED <<s3, infl>>

PutDone == /\ flushing
           /\ flushing' = FALSE
           /\ s3' = s3 + 1
           /\ IF InvalDone THEN /\ stage' = NONE /\ gen' = gen + 1
                           ELSE UNCHANGED <<stage, gen>>
           /\ UNCHANGED <<chunk, infl>>

(* kmod drops the clean copy (capacity, nocache, reset). *)
Evict == /\ ~flushing /\ chunk # NONE
         /\ chunk' = NONE
         /\ UNCHANGED <<s3, flushing, stage, gen, infl>>

PrefetchIssue == /\ infl = NONE
                 /\ infl' = [g |-> gen, v |-> NONE]
                 /\ UNCHANGED <<s3, flushing, chunk, stage, gen>>

(* S3 answers the GET with what it holds at that moment. *)
PrefetchServe == /\ infl # NONE /\ infl.v = NONE
                 /\ infl' = [infl EXCEPT !.v = s3]
                 /\ UNCHANGED <<s3, flushing, chunk, stage, gen>>

PrefetchDone == /\ infl # NONE /\ infl.v # NONE
                /\ infl' = NONE
                /\ IF GenCheck => infl.g = gen THEN stage' = infl.v
                                               ELSE UNCHANGED stage
                /\ UNCHANGED <<s3, flushing, chunk, gen>>

(* A clean miss: take the staged copy if there is one, else GET. *)
Miss == /\ ~flushing /\ chunk = NONE
        /\ IF stage # NONE THEN /\ chunk' = stage /\ stage' = NONE
                           ELSE /\ chunk' = s3 /\ UNCHANGED stage
        /\ UNCHANGED <<s3, flushing, gen, infl>>

Next == Write \/ PutDone \/ Evict \/ PrefetchIssue \/ PrefetchServe
        \/ PrefetchDone \/ Miss

Spec == Init /\ [][Next]_vars

Fresh == (~flushing /\ chunk # NONE) => chunk = s3
============================================================================
