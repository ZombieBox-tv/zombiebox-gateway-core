import YouTubeCastReceiver, { Constants } from "yt-cast-receiver";
import { Bridge } from "./bridge.mjs";
import { MemoryStore, RemotePlayer } from "./player.mjs";
import { naturalCompletion } from "./completion.mjs";
const logger = { error() {}, warn() {}, info() {}, debug() {}, setLevel() {} };

/** Owns upstream lifetime and volatile state; knows nothing about HTTP requests. */
export class Receiver {
  session = null;
  constructor(config) {
    this.config = config;
  }
  get active() {
    return this.session !== null;
  }
  get id() {
    return this.session?.id;
  }
  get expired() {
    return this.session && Date.now() - this.session.touched > 45000;
  }
  touch() {
    this.session.touched = Date.now();
  }
  async stop() {
    const old = this.session;
    this.session = null;
    if (!old) return;
    old.bridge.close();
    old.codes?.stop();
    const watchdog = setTimeout(() => process.exit(1), 10000);
    try {
      await old.receiver.stop();
    } finally {
      clearTimeout(watchdog);
    }
  }
  async start(id) {
    if (!/^[a-f0-9]{32}$/.test(id)) throw Error("invalid_receiver");
    const bridge = new Bridge(),
      player = new RemotePlayer(bridge);
    const receiver = new YouTubeCastReceiver(player, {
      dataStore: new MemoryStore(),
      logger,
      device: { name: "Zombie Box YouTube", screenName: "Zombie Box YouTube" },
      app: { enableAutoplayOnConnect: false },
      dial: {
        port: this.config.dialPort || 8096,
        prefix: "/youtube",
        corsAllowOrigins: false,
        bindToAddresses: this.config.dialAddresses,
      },
    });
    this.session = {
      id,
      bridge,
      player,
      receiver,
      touched: Date.now(),
      epoch: "",
      tvCode: "",
      state: "STARTING",
    };
    // Upstream initialization must not leave an orphan receiver after an HTTP timeout.
    const watchdog = setTimeout(() => process.exit(1), 20000);
    try {
      await receiver.start();
      const current = this.session;
      current.state = "WAITING";
      current.codes = receiver.getPairingCodeRequestService();
      current.codes.on("response", (code) => {
        if (this.session === current && /^\d[\d -]{4,30}$/.test(code)) {
          current.tvCode = code;
          current.state = "READY";
        }
      });
      current.codes.on("error", () => {
        if (this.session === current) {
          current.tvCode = "";
          current.state = "UNAVAILABLE";
        }
      });
      current.codes.start();
    } catch (error) {
      await this.stop();
      throw error;
    } finally {
      clearTimeout(watchdog);
    }
  }
  snapshot() {
    return {
      receiverId: this.session.id,
      epoch: this.session.epoch,
      state: this.session.state,
      tvCode: this.session.tvCode,
      command: this.session.bridge.pending?.command || null,
    };
  }

  suspend(epoch) {
    if (!/^[a-f0-9]{32}$/.test(epoch)) throw Error("invalid_epoch");
    this.session.epoch = epoch;
    this.session.bridge.finish(false);
    this.acknowledge({
      epoch,
      commandId: "",
      success: false,
      state: "STOPPED",
      positionMs: 0,
      durationMs: 0,
      volume: this.session.bridge.state.volume,
      muted: this.session.bridge.state.muted,
    });
  }

  acknowledge(state) {
    if ((state.epoch || "") !== this.session.epoch) throw Error("stale_epoch");
    const current = this.session;
    const epoch = current.epoch;
    const advance = naturalCompletion(current.bridge.state, state, current.bridge.pending);
    this.session.bridge.acknowledge(state);
    // Command methods notify upstream after their promises resolve. Heartbeats
    // additionally propagate local remote-control changes and natural completion.
    if (!state.commandId) {
      const status = {
        PLAYING: Constants.PLAYER_STATUSES.PLAYING,
        PAUSED: Constants.PLAYER_STATUSES.PAUSED,
        ENDED: Constants.PLAYER_STATUSES.STOPPED,
        STOPPED: Constants.PLAYER_STATUSES.STOPPED,
      }[state.state];
      void current.player
        .notifyExternalStateChange(status)
        .then(async () => {
          if (
            advance &&
            this.session === current &&
            current.epoch === epoch &&
            !current.bridge.pending &&
            current.bridge.state.state === "ENDED"
          ) {
            await current.player.next();
          }
        })
        .catch(() => {});
    }
  }
}
