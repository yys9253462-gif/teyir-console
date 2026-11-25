// Isolated component/hook contract tests: no HTTP, browser storage or production configuration.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const path = require('node:path')
const ts = require('typescript')
const root = path.resolve(__dirname, '..')
let states=[], cursor=0, effects=[], timers=[], calls=[], pollResults=[]
const react={useState(initial){const i=cursor++;if(!(i in states))states[i]=typeof initial==='function'?initial():initial;return [states[i],v=>states[i]=typeof v==='function'?v(states[i]):v]},useEffect(fn){effects.push(fn)},useRef(v){return {current:v}},useCallback(fn){return fn},useMemo(fn){return fn()}}
const jsx=(type,props)=>({type,props:props||{}})
const api={installJob:async()=>{const result=pollResults.shift();if(result instanceof Error)throw result;return result},updateSubscriptionRecord:async(id,payload)=>{calls.push(payload)}}
function load(file, extra={}) {
 let source=fs.readFileSync(path.join(root,file),'utf8')
 if(file.endsWith('SubscriptionCenter.tsx'))source+='\nexport { EditSubscriptionDialog };'
 const code=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.CommonJS,jsx:ts.JsxEmit.ReactJSX,target:ts.ScriptTarget.ES2022}}).outputText
 const exports={};const sandbox={exports,require(name){if(name==='react')return react;if(name==='react/jsx-runtime')return {jsx,jsxs:jsx};if(name.includes('/api'))return {api,loadSession:()=>({role:'admin'}),isAdminSession:()=>true};return extra[name]||new Proxy({}, {get:(_,key)=>key})},window:{setTimeout(fn){timers.push(fn);return timers.length},clearTimeout(){}},Date,console}
 vm.runInNewContext(code,sandbox,{filename:file});return exports
}
function flatten(node){if(!node||typeof node!=='object')return [];return [node,...[node.props?.children].flat(Infinity).flatMap(flatten)]}
function text(node){if(node==null||typeof node==='boolean')return '';if(typeof node==='string'||typeof node==='number')return String(node);return [node?.props?.children].flat(Infinity).map(text).join('')}
async function main(){
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
 console.log('PASS polling failure -> retry -> running -> success; unmount cancellation')
 states=[];cursor=0;effects=[]
 const {EditSubscriptionDialog}=load('src/components/SubscriptionCenter.tsx')
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
 states=[];cursor=0;effects=[]
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
