// Isolated component/hook contract tests: no HTTP, browser storage or production configuration.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const path = require('node:path')
const ts = require('typescript')
const root = path.resolve(__dirname, '..')
let states=[], cursor=0, effects=[], timers=[], calls=[], pollResults=[]
function resetHooks(){states=[];cursor=0;effects=[]}
const react={useState(initial){const bag=states;const i=cursor++;if(!(i in bag))bag[i]=typeof initial==='function'?initial():initial;return [bag[i],v=>bag[i]=typeof v==='function'?v(bag[i]):v]},useEffect(fn){effects.push(fn)},useRef(v){return {current:v}},useCallback(fn){return fn},useMemo(fn){return fn()}}
const jsx=(type,props)=>({type,props:props||{}})
const api={installJob:async()=>{const result=pollResults.shift();if(result instanceof Error)throw result;return result},updateSubscriptionRecord:async(id,payload)=>{calls.push(payload)}}
function load(file, extra={}, env={}) {
 let source=fs.readFileSync(path.join(root,file),'utf8')
 if(file.endsWith('SubscriptionCenter.tsx'))source+='\nexport { EditSubscriptionDialog };'
 if(file.endsWith('RenewDialog.tsx'))source+='\nexport { sameOriginPath };'
 const code=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.CommonJS,jsx:ts.JsxEmit.ReactJSX,target:ts.ScriptTarget.ES2022}}).outputText
 const exports={};const sandbox={exports,require(name){if(name==='react')return react;if(name==='react/jsx-runtime')return {jsx,jsxs:jsx};if(name.includes('/api'))return {api,loadSession:()=>({role:'admin'}),isAdminSession:()=>true};return extra[name]||new Proxy({}, {get:(_,key)=>key})},window:{setTimeout(fn){timers.push(fn);return timers.length},clearTimeout(){}},URL,Date,console,...env}
 vm.runInNewContext(code,sandbox,{filename:file});return exports
}
function flatten(node){if(!node||typeof node!=='object')return [];return [node,...[node.props?.children].flat(Infinity).flatMap(flatten)]}
function text(node){if(node==null||typeof node==='boolean')return '';if(typeof node==='string'||typeof node==='number')return String(node);return [node?.props?.children].flat(Infinity).map(text).join('')}
async function main(){
 // 1. 链接协议与同源相对路径安全校验：拦截 javascript:/data:/vbscript:/协议相对（//）/反斜杠（/\）/控制字符跨站绕过。
 const {safeHttpUrl,safeSameOriginPath}=load('src/lib/safeUrl.ts')
 assert.equal(safeHttpUrl('https://panel.example.com/path?a=1'), 'https://panel.example.com/path?a=1')
 assert.equal(safeHttpUrl('http://127.0.0.1:8080/renew'), 'http://127.0.0.1:8080/renew')
 assert.equal(safeHttpUrl('/sub/view/token123'), 'https://example.invalid/sub/view/token123')
 assert.equal(safeHttpUrl('//evil.example/phish'), undefined)
 assert.equal(safeHttpUrl('/\\evil.com'), undefined)
 assert.equal(safeHttpUrl('/\\evil.com/phish'), undefined)
 assert.equal(safeHttpUrl('/\t/evil.com'), undefined)
 assert.equal(safeHttpUrl('/\n/evil.com'), undefined)
 assert.equal(safeHttpUrl('/sub/view/\tabc'), undefined)
 assert.equal(safeHttpUrl('https://panel.example.com/\r\nabc'), undefined)
 assert.equal(safeHttpUrl('javascript:alert(1)'), undefined)
 assert.equal(safeHttpUrl('JAVASCRIPT:alert(1)'), undefined)
 assert.equal(safeHttpUrl('java\tscript:alert(1)'), undefined)
 assert.equal(safeHttpUrl('data:text/html,<script>alert(1)</script>'), undefined)
 assert.equal(safeHttpUrl('vbscript:msgbox(1)'), undefined)
 assert.equal(safeHttpUrl('not a url'), undefined)
 assert.equal(safeHttpUrl(''), undefined)
 assert.equal(safeHttpUrl(undefined), undefined)
 assert.equal(safeSameOriginPath('/store-api/qr.png?id=1'), '/store-api/qr.png?id=1')
 assert.equal(safeSameOriginPath('/\\evil.com/px.png'), undefined)
 assert.equal(safeSameOriginPath('//evil.com/px.png'), undefined)
 assert.equal(safeSameOriginPath('/\t/evil.com/px.png'), undefined)
 assert.equal(safeSameOriginPath('https://evil.com/px.png'), undefined)
 const {sameOriginPath}=load('src/components/RenewDialog.tsx',{'../lib/safeUrl':{safeHttpUrl,safeSameOriginPath}})
 assert.equal(sameOriginPath('/store-api/qr.png?id=1'), '/store-api/qr.png?id=1')
 assert.equal(sameOriginPath('/\\evil.com/px.png'), undefined)
 assert.equal(sameOriginPath('/\t/evil.com/px.png'), undefined)
 console.log('PASS safeHttpUrl & sameOriginPath reject dangerous schemes, backslash and control-char bypasses')

 // 2. 安装/迁移任务轮询契约：临时故障自动重试、卸载取消、401/403 立即停止轮询。
 const {useInstallJobPolling,pendingInstallJob}=load('src/lib/useInstallJobPolling.ts')
 let latest=pendingInstallJob('fixture','server'), error='', completed=0
 useInstallJobPolling(latest,v=>latest=v,v=>error=v,()=>completed++)
 const cleanup=effects.pop()();pollResults=[new Error('temporary failure'),{...latest,status:'running'},{...latest,status:'success'}]
 await timers.shift()();assert.match(error,/自动重试/);assert.equal(timers.length,1)
 await timers.shift()();assert.equal(error,'');assert.equal(timers.length,1)
 await timers.shift()();assert.equal(completed,1);assert.equal(timers.length,0);cleanup()
 effects=[];timers=[];pollResults=[{...latest,status:'running'}]
 useInstallJobPolling(pendingInstallJob('fixture2','server'),()=>{throw Error('updated after close')},()=>{},()=>{})
 const cancel=effects.pop()();cancel();await timers.shift()();assert.equal(timers.length,0)
 effects=[];timers=[];error=''
 const authErr=Object.assign(new Error('Unauthorized'),{status:401})
 pollResults=[authErr]
 useInstallJobPolling(pendingInstallJob('fixture3','server'),()=>{},v=>error=v,()=>{})
 const cleanupAuth=effects.pop()();await timers.shift()()
 assert.match(error,/无权限|已停止轮询/);assert.equal(timers.length,0);cleanupAuth()
 console.log('PASS polling failure -> retry -> running -> success; unmount cancellation; 401 stops polling')

 // 3. 人机验证脚本加载失败后自动清理残留标签，支持后续重试。
 const headChildren=[]
 const fakeDoc={
  querySelector(sel){return headChildren.find(el=>el.src&&sel.includes(el.src))||null},
  createElement(){
   const listeners={load:[],error:[]}
   const el={
    src:'',async:false,defer:false,dataset:{},
    addEventListener(type,fn){(listeners[type]||(listeners[type]=[])).push(fn)},
    removeEventListener(type,fn){const a=listeners[type];if(!a)return;const i=a.indexOf(fn);if(i>=0)a.splice(i,1)},
    emit(type){for(const fn of [...(listeners[type]||[])])fn()},
    remove(){const i=headChildren.indexOf(el);if(i>=0)headChildren.splice(i,1)},
   }
   return el
  },
  head:{appendChild(el){headChildren.push(el)}},
 }
 const fakeWin={setTimeout(){return 1},clearTimeout(){},turnstile:undefined}
 const {loadTurnstile}=load('src/lib/turnstile.ts',{},{document:fakeDoc,window:fakeWin})
 const firstLoad=loadTurnstile()
 assert.equal(headChildren.length,1)
 headChildren[0].emit('error')
 await assert.rejects(firstLoad,/人机验证加载失败/)
 assert.equal(headChildren.length,0,'failed turnstile script tag must be removed for retry')
 const secondLoad=loadTurnstile()
 assert.equal(headChildren.length,1)
 fakeWin.turnstile={render(){return 'w1'},reset(){},remove(){}}
 headChildren[0].emit('load')
 assert.equal(await secondLoad, fakeWin.turnstile)
 console.log('PASS turnstile script load failure cleans up tag and recovers on retry')

 // 4. 被墙检测面板允许将已配置的国内探测点切回「不使用」并保存取消。
 resetHooks()
 let savedProbeId=null
 api.blockChecks=async()=>({probe:{server_id:'srv-cn',name:'CN',host:'203.0.113.10'},items:[]})
 api.servers=async()=>[{id:'srv-cn',name:'CN',host:'203.0.113.10',has_credential:true}]
 api.setCNProbe=async(id)=>{savedProbeId=id}
 const {BlockCheckPanel}=load('src/components/BlockCheckPanel.tsx')
 BlockCheckPanel({notify(){}})
 // 状态槽位：0=data, 1=servers, 2=running, 3=savingProbe, 4=choosing
 states[0]={probe:{server_id:'srv-cn',name:'CN',host:'203.0.113.10'},items:[]}
 states[1]=[{id:'srv-cn',name:'CN',host:'203.0.113.10',has_credential:true}]
 cursor=0;let bp=BlockCheckPanel({notify(){}})
 const select=flatten(bp).find(n=>n.type==='select')
 assert.ok(select)
 select.props.onChange({target:{value:''}})
 assert.equal(states[4],'')
 cursor=0;bp=BlockCheckPanel({notify(){}})
 const saveBtn=flatten(bp).find(n=>n.type==='button'&&text(n)==='保存')
 assert.ok(saveBtn,'save button must appear when clearing CN probe to empty string')
 saveBtn.props.onClick()
 await new Promise(resolve=>setImmediate(resolve))
 assert.equal(savedProbeId,'')
 console.log('PASS BlockCheckPanel allows clearing configured CN probe back to unused')

 // 5. 订阅编辑弹窗：续期/改永久绝不挟带未保存字段，也不触发无关校验。
 resetHooks()
 const {EditSubscriptionDialog}=load('src/components/SubscriptionCenter.tsx',{'../lib/safeUrl':{safeHttpUrl}})
 const props={item:{id:'fixture',label:'original',ip_limit:2,user_email:'fixture@example.invalid',note:'',auto_delete:false,expires_at:0},notify(){},onClose(){},onSaved(){}}
 EditSubscriptionDialog(props)
 // Unsaved invalid device limit and destructive reset must never accompany date operations.
 states[0]='unsaved';states[2]='invalid';states[5]=true;states[6]=true
 cursor=0;const tree=EditSubscriptionDialog(props)
 const buttons=flatten(tree).filter(n=>n.type==='button')
 await buttons.find(n=>text(n)==='+30 天').props.onClick()
 assert.deepEqual(JSON.parse(JSON.stringify(calls.pop())),{duration_days:30})
 await buttons.find(n=>text(n)==='改为永久有效').props.onClick()
 assert.deepEqual(JSON.parse(JSON.stringify(calls.pop())),{expires_at:0})
 console.log('PASS renewal/perpetual payload excludes every unsaved field and skips unrelated validation')

 // 6. 节点出口设置：保存失败保持弹窗与输入域，成功后关闭。
 resetHooks()
 let fail=true
 api.nodeOutbounds=async()=>({mode:'rules',outbounds:[{name:'proxy',type:'direct'}],rules:[],builtin:[]})
 api.nodePortalAction=async()=>{if(fail)throw Error('fixture rejected')}
 const {NodeEgress}=load('src/components/NodeEgress.tsx')
 const ep={node:{id:'fixture'},notify(){}}
 NodeEgress(ep);await effects.shift()()
 // load effect launches a promise; drain the microtask queue before rendering.
 await new Promise(resolve=>setImmediate(resolve));states[0]=await api.nodeOutbounds();states[4]=true;states[5]='example.invalid'
 cursor=0;let e=NodeEgress(ep)
 let dialog=flatten(e).find(n=>typeof n.type==='function'&&n.props.onSave)
 assert.ok(dialog, JSON.stringify({states, tree:e}));await dialog.props.onSave({name:'draft',addr:'127.0.0.1:1',password:'synthetic'})
 assert.equal(states[4],true)
 let add=flatten(e).find(n=>n.type==='button'&&text(n).includes('添加规则'))
 await add.props.onClick();await Promise.resolve();assert.equal(states[5],'example.invalid')
 fail=false;await dialog.props.onSave({name:'draft'});assert.equal(states[4],false)
 console.log('PASS failed outbound save keeps dialog; failed rule save keeps domain; success closes')
}
main().catch(error=>{console.error(error);process.exitCode=1})
