import os, pty, select, time, sys
pid, fd = pty.fork()
if pid == 0:
    os.environ['TERM'] = 'xterm-256color'
    os.execv('/bin/zsh', ['/bin/zsh', '-i'])
def drain(t=1.0):
    t0=time.time(); buf=b''
    while time.time()-t0 < t:
        r,_,_ = select.select([fd], [], [], 0.1)
        if r:
            try: c = os.read(fd, 65536)
            except OSError: break
            if not c: break
            buf += c
    return buf.decode('utf-8','replace')
print('### startup ->', repr(drain(1.2)[-260:]))
os.write(fd, b'echo ALPHA\n'); print('### echo ALPHA ->', repr(drain(1.0)[-260:]))
os.write(fd, b'alias | wc -l\n'); print('### alias count ->', repr(drain(0.8)[-200:]))
os.write(fd, b'vim --version | head -1\n'); print('### vim ->', repr(drain(1.0)[-160:]))
os.write(fd, b'exit\n'); time.sleep(0.3)
