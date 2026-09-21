import { getQuickJS, shouldInterruptAfterDeadline } from "quickjs-emscripten";

// No host functions, filesystem, network, process or credentials enter this VM.
export async function evaluate(data) {
  if (typeof data.output !== "string" || data.output.length > 2 * 1024 * 1024)
    throw new Error("script_limit");
  const quickjs = await getQuickJS();
  return quickjs.evalCode(`(function(){${data.output}\n})()`, {
    memoryLimitBytes: 32 * 1024 * 1024,
    maxStackSizeBytes: 512 * 1024,
    shouldInterrupt: shouldInterruptAfterDeadline(Date.now() + 500),
  });
}
