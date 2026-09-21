import {randomBytes} from 'node:crypto';

// A bounded command mailbox. Sending an HTTP request is not playback success:
// upstream promises resolve only after Android acknowledges observed execution.
export class Bridge {
  constructor(timeoutMs=35000) {
    this.timeoutMs=timeoutMs;this.pending=null;this.lastAck='';this.closed=false;
    this.state={state:'STOPPED',positionMs:0,durationMs:0,volume:100,muted:false};
  }
  command(action, values={}) {
    if(this.closed)return Promise.resolve(false);
    if(this.pending){
      if(!['play','stop'].includes(action))return Promise.resolve(false);
      this.finish(false);
    }
    return new Promise(resolve=>{
      const command={id:randomBytes(16).toString('hex'),action,...values};
      const timer=setTimeout(()=>this.finish(false),this.timeoutMs);
      this.pending={command,resolve,timer};
    });
  }
  finish(success) {
    if(!this.pending)return;
    const pending=this.pending;this.pending=null;clearTimeout(pending.timer);
    this.lastAck=pending.command.id;pending.resolve(success);
  }
  acknowledge(value) {
    if(!value || typeof value.success!=='boolean' || !['PLAYING','PAUSED','STOPPED','ENDED','BUFFERING','FAILED'].includes(value.state) || !Number.isInteger(value.positionMs) || value.positionMs<0 || value.positionMs>604800000 || !Number.isInteger(value.durationMs) || value.durationMs<0 || value.durationMs>604800000 || !Number.isInteger(value.volume) || value.volume<0 || value.volume>100 || typeof value.muted!=='boolean')throw Error('invalid_state');
    if(value.commandId && value.commandId!==this.pending?.command.id && value.commandId!==this.lastAck)throw Error('stale_command');
    this.state={state:value.state,positionMs:value.positionMs,durationMs:value.durationMs,volume:value.volume,muted:value.muted};
    if(value.commandId===this.pending?.command.id)this.finish(value.success);
  }
  close(){this.closed=true;this.finish(false);}
}
