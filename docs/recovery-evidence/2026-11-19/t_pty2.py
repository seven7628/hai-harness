import os, pty, select, time
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
    return buf
drain(1.2)
os.write(fd, b'echo ALPHA-MARK\n'); b1 = drain(1.0)
os.write(fd, b'print -r -- "aliases=$(alias | wc -l)"\n'); b2 = drain(1.0)
os.write(fd, b'print -r -- "vim=$(command -v vim) tty=$([[ -t 1 ]] && echo yes || echo no)"\n'); b3 = drain(1.0)
os.write(fd, b'echo -n "" ; print -r -- "prompt_glyphs"\n'); b4 = drain(1.0)
def has(b, s): return ('FOUND' if s.encode() in b else 'missing')
print('ALPHA output :', has(b1,'ALPHA-MARK'))
print('alias count  :', [w for w in b2.decode('utf-8','replace').split() if w.startswith('aliases=')])
print('tty probe    :', [w for w in b3.decode('utf-8','replace').split() if w.startswith('vim=')])
print('pua glyphs in stream:', sum(1 for ch in b1.decode('utf-8','replace') if 0xE000 <= ord(ch) <= 0xF8FF))
os.write(fd, b'exit\n'); time.sleep(0.2)
