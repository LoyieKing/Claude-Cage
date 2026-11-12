export function terminalGapNote(gap) {
  if (!gap || (gap.type !== "gap" && gap.type !== "lost")) return "";
  if (gap.type === "lost") return "客机终端已丢失，不会自动重新执行命令。";
  if (gap.omitted) return "登录输出不保留，较早内容已省略。";
  const missing = Number(gap.to) - Number(gap.from);
  if (!Number.isFinite(missing) || missing <= 0) return "较早的输出已覆盖，屏幕可能不完整。";
  return `较早的输出已覆盖，缺少 ${missing} 字节，屏幕可能不完整。`;
}

export function terminalInputID() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
}

// Unknown means the bytes may already have reached the PTY. The viewer must
// show that and must not transmit them again.
export function terminalInputStatusNote(status) {
  return status === "unknown" ? "有输入是否写入终端尚不明确，不会自动重发。" : "";
}
