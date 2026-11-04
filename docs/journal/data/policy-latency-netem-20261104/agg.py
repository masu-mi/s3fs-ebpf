import re,sys,glob
R=sys.argv[1]; d={}
for f in glob.glob(R+'/*-seed*/summary.txt'):
    cond,seed=re.search(r'/(\w+)-seed(\d+)/',f).groups()
    for l in open(f):
        m=re.match(r'RESULT policy=(\S+).*?miss=\s*(\d+)\s+err=\s*(\d+).*hit_rate=\s*([\d.]+)%.*ms/access=([\d.]+)',l)
        if m: d[(m[1],cond,int(seed))]=(int(m[2]),int(m[3]),float(m[4]),float(m[5]))
pols=['observe_ops','lru_pick_ops','sieve_pick_ops','s3fifo_pick_ops','wtinylfu_pick_ops']
for p in pols:
    nd=[d[(p,'nodelay',s)] for s in (42,43,44)]; dl=[d[(p,'delay',s)] for s in (42,43,44)]
    print(p,'miss',[x[0] for x in nd],'hit%',[x[2] for x in nd],'err',sum(x[1] for x in nd+dl))
    print('   nodelay ms/acc',[x[3] for x in nd],' delay',[x[3] for x in dl],' diff',['%.2f'%(b[3]-a[3]) for a,b in zip(nd,dl)],' diff*2000/miss ms',['%.1f'%((b[3]-a[3])*2000/a[0]) for a,b in zip(nd,dl)])
