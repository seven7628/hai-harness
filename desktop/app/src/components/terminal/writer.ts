function isStep(item) {
  return typeof item === "function";
}
function terminalWriter(write) {
  let queue = [];
  let running = false;
  let closed = false;
  let pumping = false;
  function pump() {
    if (pumping || closed) return;
    pumping = true;
    try {
      while (!closed && !running && queue.length > 0) {
        const head3 = queue[0];
        if (isStep(head3)) {
          queue.shift();
          try {
            head3();
          } catch {
          }
          continue;
        }
        const batch2 = [];
        while (batch2.length < queue.length) {
          const next5 = queue[batch2.length];
          if (isStep(next5)) break;
          batch2.push(next5);
        }
        queue.splice(0, batch2.length);
        let total = 0;
        for (const chunk of batch2) total += chunk.length;
        if (total === 0) continue;
        const merged = new Uint8Array(total);
        let offset = 0;
        for (const chunk of batch2) {
          merged.set(chunk, offset);
          offset += chunk.length;
        }
        running = true;
        let settled = false;
        write(merged, () => {
          if (settled) return;
          settled = true;
          if (closed) return;
          running = false;
          pump();
        });
      }
    } finally {
      pumping = false;
    }
  }
  return {
    push(item) {
      if (closed) return;
      queue.push(item);
      pump();
    },
    // 在飞的 write 取消不了，但它的 done 到这里已经没有意义
    clear() {
      closed = true;
      queue = [];
    }
  };
}
