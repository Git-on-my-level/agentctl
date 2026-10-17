#!/usr/bin/env python3
import json, os, sys
mode = os.environ.get('ACP_FIXTURE_MODE', 'normal')
sid = 'fixture-session'
prompt_id = None
def send(v):
    print(json.dumps(dict(jsonrpc='2.0', **v)), flush=True)
def result(i,v): send(dict(id=i,result=v))
def update(text): send(dict(method='session/update',params=dict(sessionId=sid,update=dict(sessionUpdate='agent_message_chunk',content=dict(type='text',text=text)))))
def finish():
    update('Hello '); update('world')
    send(dict(method='session/update',params=dict(sessionId=sid,update=dict(sessionUpdate='tool_call',toolCallId='private-call-id',title='private tool text',rawInput={'private':'input'},content=[dict(type='content',content=dict(type='text',text='PRIVATE TOOL OUTPUT'))]))))
    if mode == 'rpc-error': send(dict(id=prompt_id,error=dict(code=-32000,message='PRIVATE RPC ERROR')))
    else: result(prompt_id,dict(stopReason=mode if mode in ['refusal','max_tokens','max_turn_requests','cancelled','future_reason'] else 'end_turn'))
for line in sys.stdin:
    v=json.loads(line); method=v.get('method'); i=v.get('id')
    if method=='initialize':
        assert v['params']['protocolVersion']==1
        assert v['params']['clientCapabilities']=={}
        result(i,dict(protocolVersion=1,agentCapabilities=dict(loadSession=mode!='no-load')))
    elif method=='session/new':
        assert os.path.isabs(v['params']['cwd']) and v['params']['mcpServers']==[]
        result(i,dict(sessionId=sid))
    elif method=='session/load':
        assert mode!='no-load' and v['params']['sessionId']==sid
        update('REPLAY MUST NOT BECOME RESULT');result(i,{})
    elif method=='session/prompt':
        if mode=='eof': sys.exit(0)
        if mode=='malformed': print('not-json',flush=True); sys.exit(0)
        prompt_id=i
        assert v['params']['sessionId']==sid
        assert v['params']['prompt'][0]['type']=='text'
        if mode in ['cancel','cancel-hang']: continue
        if mode in ['permission','fs']:
            if mode=='permission': send(dict(id='permission-1',method='session/request_permission',params=dict(sessionId=sid,toolCall=dict(toolCallId='tool-1'),options=[dict(optionId='yes',kind='allow_once',name='Allow once')])) )
            else: send(dict(id='fs-1',method='fs/read_text_file',params=dict(sessionId=sid,path='/secret')))
        else: finish()
    elif method=='session/cancel':
        if mode=='cancel-hang': continue
        assert 'id' not in v
        result(prompt_id,dict(stopReason='cancelled'))
    elif i=='permission-1':
        outcome=v['result']['outcome']
        expected=os.environ.get('ACP_EXPECT_PERMISSION','cancelled')
        assert outcome['outcome']==expected, outcome
        if expected=='selected': assert outcome['optionId']=='yes'
        finish()
    elif i=='fs-1':
        assert v['error']['code']==-32601
        finish()
    else: raise AssertionError(v)
