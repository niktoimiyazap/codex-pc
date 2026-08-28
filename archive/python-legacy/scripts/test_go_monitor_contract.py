import json, subprocess, pathlib, hashlib, os
root=pathlib.Path(r'C:\Users\niktoimiya\Desktop\projects\codex-mcp-router')
p=subprocess.Popen([str(root/'dist'/'codexpc-go.exe')],cwd=root,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,encoding='utf-8',bufsize=1)
seq=0
def call(method,params=None):
    global seq
    seq+=1
    m={'jsonrpc':'2.0','id':seq,'method':method}
    if params is not None:m['params']=params
    p.stdin.write(json.dumps(m,separators=(',',':'))+'\n');p.stdin.flush()
    while True:
        r=json.loads(p.stdout.readline())
        if r.get('id')==seq:return r
call('initialize',{'protocolVersion':'2025-06-18','capabilities':{},'clientInfo':{'name':'monitor-contract-test','version':'1'}})
path=root/'.local'/'monitor-contract.txt'
call('tools/call',{'name':'fs_write_file','arguments':{'path':str(path),'content':'alpha\n'}})
r=call('tools/call',{'name':'fs_read_file','arguments':{'path':str(path)}})
h=r['result']['structuredContent']['sha256']
call('tools/call',{'name':'fs_edit_file','arguments':{'path':str(path),'expected_sha256':h,'edits':[{'old_text':'alpha','new_text':'beta'}]}})
call('tools/call',{'name':'fs_remove','arguments':{'path':str(path)}})
p.stdin.close();p.terminate()
print('ok')
