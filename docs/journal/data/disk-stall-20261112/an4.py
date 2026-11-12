import glob,statistics as st,sys
rows=[]
for f in sorted(glob.glob('/tmp/phase/b4/*.iters')):
    tag=f.split('/')[-1][:-6]
    it=[tuple(map(float,l.split())) for l in open(f)]
    sm=[l.split() for l in open(f[:-6]+'.samples')]
    sm=[(float(a[0]),)+tuple(float(x) for x in a[1:]) for a in sm if len(a)>=10]
    med=st.median(d for _,d in it)
    slow=[(t,d) for t,d in it if d>2*med]
    # per-second disk stats
    per=[]
    for a,b in zip(sm,sm[1:]):
        dt=b[0]-a[0]; w=b[1]-a[1]; sec=b[2]-a[2]; msw=b[3]-a[3]; msio=b[4]-a[4]
        per.append((b[0], sec*512/1e6/dt, (msw/w if w>0 else 0), msio/10/dt, b[5], b[6], b[7]))
    def at(t):  # sample whose interval covers t..t+? : mean over samples in [t, t+d]
        return None
    def stats(t0,t1):
        s=[p for p in per if t0-1<=p[0]<=t1+1]
        if not s: return (0,0,0,0)
        return (max(p[1] for p in s), max(p[2] for p in s), max(p[3] for p in s), max(p[6] for p in s))
    tot=sum(d for _,d in it)/1000
    allp=[p for p in per]
    print(f"{tag:22s} med={med:5.0f} n_slow={len(slow):2d} slowtime={sum(d for _,d in slow)/1000:5.1f}s/{tot:5.1f}s  disk: mean_w={st.mean(p[1] for p in allp):5.0f}MB/s max_await={max(p[2] for p in allp):6.1f}ms  psi_io={allp[-1][6]-allp[0][6]:.0f}us")
    # during slow windows vs not
    if slow:
        ws=[stats(t,t+d/1000) for t,d in slow]
        print("    slow-window max: w=%.0fMB/s await=%.1fms util=%.0f%%  | typical window await max: %.1f" % (st.mean(w[0] for w in ws), st.mean(w[1] for w in ws), st.mean(w[2] for w in ws), st.median(p[2] for p in allp)))
