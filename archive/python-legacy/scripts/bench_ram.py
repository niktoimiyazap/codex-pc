import json, os, subprocess, time
from pathlib import Path
ROOT=Path(r'C:\Users\niktoimiya\Desktop\projects\codex-mcp-router')
ENV=os.environ.copy(); ENV['PYTHONIOENCODING']='utf-8'

def rss_tree(pid):
    ps=f"$ids=@({pid});$todo=@({pid});while($todo.Count){{$x=$todo[0];$todo=@($todo|Select-Object -Skip 1);$c=@(Get-CimInstance Win32_Process|? {{$_.ParentProcessId -eq $x}}|% ProcessId);$ids+=$c;$todo+=$c}};(Get-Process -Id $ids -ErrorAction SilentlyContinue|Measure-Object WorkingSet64 -Sum).Sum"
    out=subprocess.check_output(['powershell','-NoProfile','-Command',ps],text=True).strip()
    return int(float(out or 0))/1024/1024

def run(cmd):
    p=subprocess.Popen(cmd,cwd=ROOT,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,encoding='utf-8',env=ENV,bufsize=1)
    msg={'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':'2025-06-18','capabilities':{},'clientInfo':{'name':'ram','version':'1'}}}
    p.stdin.write(json.dumps(msg)+'\n'); p.stdin.flush(); p.stdout.readline()
    p.stdin.write(json.dumps({'jsonrpc':'2.0','method':'notifications/initialized','params':{}})+'\n'); p.stdin.flush()
    time.sleep(1)
    mb=rss_tree(p.pid)
    p.terminate()
    try:p.wait(timeout=3)
    except:p.kill()
    return round(mb,1)

print(json.dumps({'python_mb':run(['python','-m','codexpc_connector']),'go_mb':run([str(ROOT/'dist'/'codexpc-go.exe')])}))
