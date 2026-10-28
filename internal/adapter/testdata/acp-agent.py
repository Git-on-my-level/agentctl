#!/usr/bin/env python3
import json, os, sys
mode = os.environ.get('ACP_FIXTURE_MODE', 'normal')
sid = 'fixture-session'
prompt_id = None
config_mode = os.environ.get('ACP_CONFIG_MODE', 'normal')
# Select options shaped like Devin's: local model, cloud devin_version, repos.
options = {'model': 'swe-2-high', 'devin_version': 'devin-2-5', 'repos': ''}
values = {'model': ['swe-2-high', 'gpt-6-luna-medium', 'fixture-model'], 'devin_version': ['devin-2-5', 'devin-swe-2-high'], 'repos': ['owner/repo']}
def config_options():
    return [dict(id=k, type='select', currentValue=v, options=[dict(value=x, name=x) for x in values[k]]) for k, v in options.items()]
def send(v):
    print(json.dumps(dict(jsonrpc='2.0', **v)), flush=True)
def result(i,v): send(dict(id=i,result=v))
def update(text): send(dict(method='session/update',params=dict(sessionId=sid,update=dict(sessionUpdate='agent_message_chunk',content=dict(type='text',text=text)))))
def tool(kind='tool_call'):
    send(dict(method='session/update',params=dict(sessionId=sid,update=dict(sessionUpdate=kind,toolCallId='private-call-id',title='private tool text',rawInput={'private':'input'},content=[dict(type='content',content=dict(type='text',text='PRIVATE TOOL OUTPUT'))]))))
def permission():
    options=[dict(optionId='yes',kind='allow_once',name='Allow once')]
    if mode!='permission-no-reject': options.append(dict(optionId='no',kind='reject_once',name='Reject once'))
    if mode=='permission-no-allow': options=options[1:]
    send(dict(id='permission-1',method='session/request_permission',params=dict(sessionId='wrong-session' if mode=='permission-wrong-session' else sid,toolCall=dict(toolCallId='tool-1'),options=options)))
def finish():
    if mode in ['trailing','trailing-split','silent','settle-cancel']:
        # The cloud relay can answer end_turn before the turn's final message.
        tool()
        if mode=='settle-cancel': update('Hello world')
        result(prompt_id,dict(stopReason='end_turn',_meta={'cognition.ai/session':{'cognition.ai/url':'https://app.devin.ai/sessions/abc123'}}))
        if mode=='trailing': update('Hello world')
        if mode=='trailing-split': update('Hello '); update('world')
        return
    if mode=='interleaved':
        update('Checking '); update('now.')
        tool()
        update('Still checking.')
        tool('tool_call_update')
        update('FILES='); update('4')
        # A trailing tool update without a subsequent message must not erase it.
        tool('tool_call_update')
        result(prompt_id,dict(stopReason='end_turn'))
        return
    update('Hello '); update('world')
    tool()
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
        result(i,dict(sessionId=sid) if config_mode=='absent' else dict(sessionId=sid,configOptions=config_options()))
    elif method=='session/load':
        assert mode!='no-load' and v['params']['sessionId']==sid
        update('REPLAY MUST NOT BECOME RESULT')
        # A loaded session reports its options only through an update.
        # Like Devin cloud, a loaded session keeps its repository but lists none.
        if config_mode!='absent':
            options['repos']='owner/repo'; values['repos']=[]
            send(dict(method='session/update',params=dict(sessionId=sid,update=dict(sessionUpdate='config_option_update',configOptions=config_options()))))
        result(i,{})
    elif method=='session/set_config_option':
        assert v['params']['sessionId']==sid
        if mode=='cancel-config':
            held=i; continue
        if config_mode=='reject': send(dict(id=i,error=dict(code=-32602,message='rejected'))); continue
        if config_mode!='ignore': options[v['params']['configId']]=v['params']['value']
        result(i,dict(configOptions=config_options()))
    elif method=='session/prompt':
        if mode=='eof': sys.exit(0)
        if mode=='malformed': print('not-json',flush=True); sys.exit(0)
        prompt_id=i
        assert v['params']['sessionId']==sid
        assert v['params']['prompt'][0]['type']=='text'
        if mode in ['cancel','cancel-hang','cancel-permission']: continue
        if mode=='cancel-config':
            # A prompt after a recorded cancel is the bug under test.
            open(os.environ['ACP_PROMPT_MARKER'],'w').write('prompted')
            continue
        if mode.startswith('permission'): permission()
        elif mode=='fs': send(dict(id='fs-1',method='fs/read_text_file',params=dict(sessionId=sid,path='/secret')))
        else: finish()
    elif method=='session/cancel':
        if mode in ['cancel-hang','settle-cancel']: continue
        if mode=='cancel-config':
            options[v['params'].get('configId','model')]='gpt-6-luna-medium'
            result(held,dict(configOptions=config_options()))
            continue
        assert 'id' not in v
        if mode=='cancel-permission':
            permission()
            continue
        result(prompt_id,dict(stopReason='cancelled'))
    elif i=='permission-1':
        outcome=v['result']['outcome']
        expected='cancelled' if mode in ['permission-no-reject','cancel-permission'] else os.environ.get('ACP_EXPECT_PERMISSION','selected')
        assert outcome['outcome']==expected, outcome
        if expected=='selected': assert outcome['optionId']==os.environ.get('ACP_EXPECT_OPTION','no'), outcome
        if mode=='cancel-permission': result(prompt_id,dict(stopReason='cancelled'))
        else: finish()
    elif i=='fs-1':
        assert v['error']['code']==-32601
        finish()
    else: raise AssertionError(v)
