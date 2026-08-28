import json, os, subprocess, time, statistics, sys
from pathlib import Path

ROOT=Path(r'C:\Users\niktoimiya\Desktop\projects\codex-mcp-router')
PY=[r'C:\Windows\System32\cmd.exe','/d','/c',str(ROOT/'wrapper-python.cmd')]
GO=[str(ROOT/'dist'/'codexpc-go.exe')]
ENV=os.environ.copy(); ENV['PYTHONIOENCODING']='utf-8'

class MCP:
    def __init__(self, cmd):
        self.p=subprocess.Popen(cmd,cwd=ROOT,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,encoding='utf-8',env=ENV,bufsize=1)
        self.next=1
    def call(self, method, params=None):
        i=self.next; self.next+=1
        msg={'jsonrpc':'2.0','id':i,'method':method}
        if params is not None: msg['params']=params
        self.p.stdin.write(json.dumps(msg,separators=(',',':'))+'\n'); self.p.stdin.flush()
        while True:
            line=self.p.stdout.readline()
            if not line: raise RuntimeError('process ended: '+self.p.stderr.read()[:1000])
            r=json.loads(line)
            if r.get('id')==i: return r
    def notify(self, method, params=None):
        msg={'jsonrpc':'2.0','method':method}
        if params is not None: msg['params']=params
        self.p.stdin.write(json.dumps(msg,separators=(',',':'))+'\n'); self.p.stdin.flush()
    def close(self):
        try: self.p.stdin.close()
        except: pass
        try: self.p.terminate(); self.p.wait(timeout=3)
        except: self.p.kill()

def tool(m,name,args):
    return m.call('tools/call',{'name':name,'arguments':args})

def rss_tree(pid):
    ps=f"$ids=@({pid});$todo=@({pid});while($todo.Count){{$x=$todo[0];$todo=@($todo|Select-Object -Skip 1);$c=@(Get-CimInstance Win32_Process|? {{$_.ParentProcessId -eq $x}}|% ProcessId);$ids+=$c;$todo+=$c}};(Get-Process -Id $ids -ErrorAction SilentlyContinue|Measure-Object WorkingSet64 -Sum).Sum"
    try:
        out=subprocess.check_output(['powershell','-NoProfile','-Command',ps],text=True,timeout=5).strip()
        return int(float(out or 0))
    except: return 0

def one(label,cmd):
    t=time.perf_counter(); m=MCP(cmd)
    init=m.call('initialize',{'protocolVersion':'2025-06-18','capabilities':{},'clientInfo':{'name':'bench','version':'1'}})
    m.notify('notifications/initialized',{})
    startup=(time.perf_counter()-t)*1000
    vals={}
    for name,args,n in [
      ('connector_status',{},12),
      ('fs_read_file',{'path':str(ROOT/'go.mod')},10),
      ('command_exec',{'command':['cmd','/d','/c','echo bench']},5),
      ('computer',{'action':'screen_info'},8),
      ('mcp_discover',{'query':'github','limit':5},4),
    ]:
        xs=[]
        for _ in range(n):
            a=time.perf_counter(); r=tool(m,name,args); xs.append((time.perf_counter()-a)*1000)
            if 'error' in r: raise RuntimeError(f'{label} {name}: {r}')
        vals[name]=statistics.median(xs)
    rss=rss_tree(m.p.pid)
    m.close()
    return {'startup_ms':startup,'rss_tree_mb':rss/1024/1024,**{k+'_ms':v for k,v in vals.items()}}

results={}
for label,cmd in [('python',PY),('go',GO)]:
    runs=[]
    for i in range(3):
        try: runs.append(one(label,cmd))
        except Exception as e:
            print(json.dumps({'error':label,'detail':str(e)},ensure_ascii=False));sys.exit(1)
    results[label]={k:statistics.median([r[k] for r in runs]) for k in runs[0]}
print(json.dumps(results,indent=2,ensure_ascii=False))
