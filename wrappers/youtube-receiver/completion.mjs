// Natural completion must not advance on stop, replayed heartbeats or pending commands.
export function naturalCompletion(previous, current, pending) {
  return (
    !pending &&
    !current.commandId &&
    current.state === "ENDED" &&
    ["PLAYING", "PAUSED", "BUFFERING"].includes(previous.state)
  );
}
