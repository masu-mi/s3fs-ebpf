import os,sys,time
p=sys.argv[1]
fd=os.open(p,os.O_WRONLY)
b=os.urandom(4096)
t0=time.perf_counter(); os.pwrite(fd,b,5*1024*1024); t1=time.perf_counter(); os.fsync(fd); t2=time.perf_counter()
print("write=%.1f fsync=%.1f ms"%((t1-t0)*1e3,(t2-t1)*1e3))
