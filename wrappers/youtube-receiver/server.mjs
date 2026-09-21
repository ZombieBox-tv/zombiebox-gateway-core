import http from 'node:http';
import {readFile} from 'node:fs/promises';
import {timingSafeEqual} from 'node:crypto';
import YouTubeCastReceiver, {Player, DataStore, Constants} from 'yt-cast-receiver';
import {Bridge} from './bridge.mjs';

const config=JSON.parse(await readFile(process.env.ZOMBIE_YOUTUBE_RECEIVER_CONFIG || '/config/receiver.json','utf8'));
if(typeof config.token!=='string' || config.token.length<32)throw Error('Private configuration required');
// Upstream uses fetch internally without a caller-supplied transport. Bound it
// inside this dedicated process; failures never affect the gateway process.
const upstreamFetch=globalThis.fetch;
globalThis.fetch=(input,init={})=>upstreamFetch(input,{...init,signal:init.signal?AbortSignal.any([init.signal,AbortSignal.timeout(30000)]):AbortSignal.timeout(30000)});
class MemoryStore extends DataStore {
  values=new Map();
  async get(key){return this.values.get(key)??null;}
  async set(key,value){if(this.values.size>=128&&!this.values.has(key))throw Error('store_full');this.values.set(key,value);}
}
class RemotePlayer extends Player {
  constructor(bridge){super();this.bridge=bridge;}
  doPlay(video,position){if(!/^[A-Za-z0-9_-]{11}$/.test(video.id)||!Number.isFinite(position)||position<0||position>604800)return Promise.resolve(false);return this.bridge.command('play',{videoId:video.id,positionMs:Math.round(position*1000)});}
  doPause(){return this.bridge.command('pause');}
  doResume(){return this.bridge.command('resume');}
  doStop(){return this.bridge.command('stop');}
  doSeek(position){if(!Number.isFinite(position)||position<0||position>604800)return Promise.resolve(false);return this.bridge.command('seek',{positionMs:Math.round(position*1000)});}
  doSetVolume(volume){return this.bridge.command('volume',{volume:Math.max(0,Math.min(100,Math.round(volume.level))),muted:!!volume.muted});}
  async doGetVolume(){return {level:this.bridge.state.volume,muted:this.bridge.state.muted};}
  async doGetPosition(){return this.bridge.state.positionMs/1000;}
  async doGetDuration(){return this.bridge.state.durationMs/1000;}
}
const logger={error(){},warn(){},info(){},debug(){},setLevel(){}};
let session=null,changing=false;
async function stop(){
  const old=session;session=null;if(!old)return;
  old.bridge.close();old.codes?.stop();
  const watchdog=setTimeout(()=>process.exit(1),10000);
  try{await old.receiver.stop();}finally{clearTimeout(watchdog);}
}
async function start(id){
  if(!/^[a-f0-9]{32}$/.test(id))throw Error('invalid_receiver');
  const bridge=new Bridge(),player=new RemotePlayer(bridge);
  const receiver=new YouTubeCastReceiver(player,{dataStore:new MemoryStore(),logger,
    device:{name:'Zombie Box YouTube',screenName:'Zombie Box YouTube'},
    app:{enableAutoplayOnConnect:false},
    dial:{port:config.dialPort||8096,prefix:'/youtube',corsAllowOrigins:false,bindToAddresses:config.dialAddresses}});
  session={id,bridge,player,receiver,touched:Date.now(),tvCode:'',state:'STARTING'};
  // Upstream initialization must not leave an orphan receiver after an HTTP timeout.
  const watchdog=setTimeout(()=>process.exit(1),20000);
  try {
    await receiver.start();
    const current=session;
    current.state='WAITING';current.codes=receiver.getPairingCodeRequestService();
    current.codes.on('response',code=>{if(session===current && /^\d[\d -]{4,30}$/.test(code)){current.tvCode=code;current.state='READY';}});
    current.codes.on('error',()=>{if(session===current){current.tvCode='';current.state='UNAVAILABLE';}});
    current.codes.start();
  }catch(error){await stop();throw error;}finally{clearTimeout(watchdog);}
}
async function body(req){let raw='';for await(const chunk of req){raw+=chunk;if(Buffer.byteLength(raw)>8192)throw Error('request_too_large');}return JSON.parse(raw||'{}');}
function json(res,status,data){res.writeHead(status,{'Content-Type':'application/json','Cache-Control':'no-store','X-Content-Type-Options':'nosniff'});res.end(JSON.stringify(data));}
function snapshot(){return {receiverId:session.id,state:session.state,tvCode:session.tvCode,command:session.bridge.pending?.command||null};}
const server=http.createServer(async(req,res)=>{
  const supplied=Buffer.from(req.headers.authorization||''),expected=Buffer.from('Bearer '+config.token);
  if(supplied.length!==expected.length||!timingSafeEqual(supplied,expected)){json(res,401,{error:'unauthorized'});return;}
  if(req.method==='GET'&&req.url==='/health'){json(res,200,{available:true,active:session!==null});return;}
  if(changing){json(res,409,{error:'receiver_busy'});return;}
  try{
    if(req.method==='POST'&&req.url==='/receiver'){
      if(session){json(res,409,{error:'receiver_busy'});return;}
      changing=true;try{const value=await body(req);await start(value.receiverId);json(res,201,snapshot());}finally{changing=false;}return;
    }
    const match=req.url.match(/^\/receiver\/([a-f0-9]{32})(\/state)?$/);
    if(!match||!session||session.id!==match[1]){json(res,404,{error:'receiver_not_found'});return;}
    session.touched=Date.now();
    if(req.method==='GET'&&!match[2]){json(res,200,snapshot());return;}
    if(req.method==='POST'&&match[2]){
      const state=await body(req);session.bridge.acknowledge(state);
      // Command methods notify upstream after their promises resolve. Heartbeats
      // additionally propagate local remote-control changes and natural completion.
      if(!state.commandId){const status={PLAYING:Constants.PLAYER_STATUSES.PLAYING,PAUSED:Constants.PLAYER_STATUSES.PAUSED,ENDED:Constants.PLAYER_STATUSES.STOPPED,STOPPED:Constants.PLAYER_STATUSES.STOPPED}[state.state];void session.player.notifyExternalStateChange(status).catch(()=>{});}
      json(res,200,{accepted:true});return;
    }
    if(req.method==='DELETE'&&!match[2]){changing=true;try{await stop();json(res,200,{closed:true});}finally{changing=false;}return;}
    json(res,405,{error:'method_not_allowed'});
  }catch{json(res,502,{error:'receiver_unavailable'});}
});
server.requestTimeout=25000;server.headersTimeout=5000;server.maxConnections=8;
const reaper=setInterval(()=>{if(session&&!changing&&Date.now()-session.touched>45000){changing=true;void stop().finally(()=>{changing=false;});}},5000);
server.listen(config.port||8095,config.listen||'0.0.0.0');
for(const signal of ['SIGTERM','SIGINT'])process.on(signal,()=>{clearInterval(reaper);server.close();void stop().finally(()=>process.exit(0));});
