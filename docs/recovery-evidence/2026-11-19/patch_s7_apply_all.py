# Driver: replay the parent's apply scripts against a target ROOT.
import os, re, sys, subprocess

def convert(src, dst):
    s = open(src, encoding='utf-8').read()
    s = s.replace("ROOT = '/Users/admin/workspace/go-code'", "ROOT = %r" % dst)
    open(dst,'w',encoding='utf-8').write(s)

if __name__ == '__main__':
    dst = sys.argv[1]
    for i in range(1,7):
        src = f'/private/tmp/patch_s7_apply{i if i>1 else ""}.py'
        src = f'/private/tmp/patch_s7_apply{"" if i==1 else i}.py'
        if not os.path.exists(src): print('missing', src); continue
        tmp = f'/private/tmp/_apply{i}.py'
        convert(src, tmp)
        r = subprocess.run(['python3','-I',tmp], capture_output=True, text=True)
        print(f'##### apply{i} rc={r.returncode}')
        print(r.stdout[-3000:])
        if r.stderr: print('STDERR', r.stderr[-2000:])
