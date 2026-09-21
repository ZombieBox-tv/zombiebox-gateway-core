import { Player, DataStore } from "yt-cast-receiver";
export class MemoryStore extends DataStore {
  values = new Map();
  async get(key) {
    return this.values.get(key) ?? null;
  }
  async set(key, value) {
    if (this.values.size >= 128 && !this.values.has(key)) throw Error("store_full");
    this.values.set(key, value);
  }
}
export class RemotePlayer extends Player {
  constructor(bridge) {
    super();
    this.bridge = bridge;
  }
  doPlay(video, position) {
    if (
      !/^[A-Za-z0-9_-]{11}$/.test(video.id) ||
      !Number.isFinite(position) ||
      position < 0 ||
      position > 604800
    )
      return Promise.resolve(false);
    return this.bridge.command("play", {
      videoId: video.id,
      positionMs: Math.round(position * 1000),
    });
  }
  doPause() {
    return this.bridge.command("pause");
  }
  doResume() {
    return this.bridge.command("resume");
  }
  doStop() {
    return this.bridge.command("stop");
  }
  doSeek(position) {
    if (!Number.isFinite(position) || position < 0 || position > 604800)
      return Promise.resolve(false);
    return this.bridge.command("seek", { positionMs: Math.round(position * 1000) });
  }
  doSetVolume(volume) {
    return this.bridge.command("volume", {
      volume: Math.max(0, Math.min(100, Math.round(volume.level))),
      muted: !!volume.muted,
    });
  }
  async doGetVolume() {
    return { level: this.bridge.state.volume, muted: this.bridge.state.muted };
  }
  async doGetPosition() {
    return this.bridge.state.positionMs / 1000;
  }
  async doGetDuration() {
    return this.bridge.state.durationMs / 1000;
  }
}
