// speech.ts 封装文字转语音接口。
//
// 只做「把这段文字念出来」。念的内容永远是屏幕上已经显示的原文，
// 不做任何改写——听到的和看到的必须是同一句话。

const basePath = "/api/v1";

/**
 * synthesizeSpeech 把一段文本合成成音频，返回可直接播放的对象 URL。
 *
 * 调用方用完必须 URL.revokeObjectURL，否则每念一句都会漏一份音频在内存里。
 */
export async function synthesizeSpeech(text: string, signal?: AbortSignal): Promise<string> {
  const response = await fetch(`${basePath}/speech`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text }),
    signal,
  });
  if (!response.ok) {
    // 后端在失败时返回的是 JSON 错误体而不是音频，这里尽量取出可读信息。
    let message = "语音合成失败";
    try {
      const body = (await response.json()) as { message?: string };
      if (body?.message) message = body.message;
    } catch {
      // 解析不出来就用兜底文案。
    }
    throw new Error(message);
  }
  return URL.createObjectURL(await response.blob());
}
