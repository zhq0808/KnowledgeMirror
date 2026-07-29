// 录音转码：把浏览器录出来的压缩音频转成 16 kHz 单声道 PCM16 WAV。
//
// 为什么必须有这一步：MediaRecorder 只能录出 webm/opus、ogg/opus、mp4/aac 这几种压缩格式，
// 而 MiMo ASR 只接受 wav 和 mp3。两边没有交集，所以转码不是优化项，是这条链路能不能通的前提。
//
// 为什么在录完之后转、而不是用 AudioWorklet 边录边采 PCM：
// 录制阶段保持 MediaRecorder 不动，浏览器兼容性和权限处理都沿用已经验证过的老路径；
// 转码交给 decodeAudioData + OfflineAudioContext，重采样由浏览器内部完成，
// 比自己写线性插值降采样质量高，代码也少得多。

// TARGET_SAMPLE_RATE 取 16 kHz：语音识别模型的标准输入采样率，
// 再高只会白白撑大体积（PCM16 单声道下每秒 32 KB），对识别准确率没有帮助。
const TARGET_SAMPLE_RATE = 16000;

export const WAV_MIME_TYPE = "audio/wav";

type AudioContextConstructor = new (contextOptions?: AudioContextOptions) => AudioContext;
type OfflineAudioContextConstructor = new (
  numberOfChannels: number,
  length: number,
  sampleRate: number,
) => OfflineAudioContext;

interface LegacyAudioWindow {
  webkitAudioContext?: AudioContextConstructor;
  webkitOfflineAudioContext?: OfflineAudioContextConstructor;
}

function getAudioContextConstructor(): AudioContextConstructor | null {
  if (typeof window === "undefined") return null;
  const legacy = window as unknown as LegacyAudioWindow;
  return window.AudioContext ?? legacy.webkitAudioContext ?? null;
}

function getOfflineAudioContextConstructor(): OfflineAudioContextConstructor | null {
  if (typeof window === "undefined") return null;
  const legacy = window as unknown as LegacyAudioWindow;
  return window.OfflineAudioContext ?? legacy.webkitOfflineAudioContext ?? null;
}

/** isWavEncodingSupported 判断当前浏览器能否完成转码，供录音入口提前禁用。 */
export function isWavEncodingSupported(): boolean {
  return getAudioContextConstructor() !== null && getOfflineAudioContextConstructor() !== null;
}

/**
 * recordingUnavailableReason 返回录音不可用的原因；可用时返回 null。
 *
 * 单独抽出来是因为最容易撞上的那种失败会伪装成「浏览器太旧」：
 * Chrome / Edge 只在**安全来源**（https、localhost、127.0.0.1）下挂载
 * `navigator.mediaDevices`，用 http://<局域网 IP>:5173 打开时它整个是 undefined。
 * 此时提示用户去升级浏览器，会把人引到完全错误的方向上，所以这两种情况必须分开报。
 */
export function recordingUnavailableReason(): string | null {
  if (typeof window === "undefined") return "当前环境不支持录音";

  if (!navigator.mediaDevices?.getUserMedia) {
    if (!window.isSecureContext) {
      const localhostURL = `http://localhost${window.location.port ? `:${window.location.port}` : ""}`;
      return `浏览器只在安全来源下开放麦克风：当前是 ${window.location.origin}，请改用 ${localhostURL} 打开，或给站点配上 HTTPS`;
    }
    return "当前浏览器不支持麦克风采集，请使用最新版 Chrome、Edge 或 Safari";
  }

  if (!("MediaRecorder" in window)) {
    return "当前浏览器不支持录音，请使用最新版 Chrome、Edge 或 Safari";
  }

  if (!isWavEncodingSupported()) {
    return "当前浏览器不支持音频转码，请使用最新版 Chrome、Edge 或 Safari";
  }

  return null;
}

/**
 * audioFileExtension 按 MIME 推断上传时的文件名后缀。
 *
 * 只影响服务端与供应商按扩展名猜测编码，不作为业务文件名使用；
 * 转码之后走的都是 wav 分支，其余分支留给尚未转码的调用方兜底。
 */
export function audioFileExtension(mimeType: string): string {
  const normalized = mimeType.toLowerCase();
  if (normalized.includes("wav")) return "wav";
  if (normalized.includes("mpeg") || normalized.includes("mp3")) return "mp3";
  if (normalized.includes("mp4")) return "mp4";
  if (normalized.includes("ogg")) return "ogg";
  return "webm";
}

/**
 * encodeRecordingAsWav 把一段录音转成 16 kHz 单声道 WAV。
 *
 * 抛出的错误信息直接面向用户：这一步失败时用户能做的只有重录或改用打字，
 * 报“解码失败”之类的技术词只会让人以为系统坏了。
 */
export async function encodeRecordingAsWav(
  recording: Blob,
  targetSampleRate: number = TARGET_SAMPLE_RATE,
): Promise<Blob> {
  const AudioContextCtor = getAudioContextConstructor();
  const OfflineAudioContextCtor = getOfflineAudioContextConstructor();
  if (!AudioContextCtor || !OfflineAudioContextCtor) {
    throw new Error("当前浏览器不支持音频转码，请换用最新版 Chrome 或 Edge");
  }
  if (recording.size === 0) {
    throw new Error("这段录音是空的，请重新录一次");
  }

  const arrayBuffer = await recording.arrayBuffer();

  const decodeContext = new AudioContextCtor();
  let decoded: AudioBuffer;
  try {
    decoded = await decodeContext.decodeAudioData(arrayBuffer);
  } catch {
    throw new Error("这段录音没能转成可识别的格式，请重录或直接打字");
  } finally {
    // 不 await：关闭失败不影响已经拿到的数据，也不该阻塞用户。
    void decodeContext.close();
  }

  const frameCount = Math.max(1, Math.ceil(decoded.duration * targetSampleRate));
  // 目标声道数写死 1：Web Audio 会按规范自动把多声道混成单声道，
  // 采样率不一致时 AudioBufferSourceNode 也会自动重采样，两件事一次做完。
  const offline = new OfflineAudioContextCtor(1, frameCount, targetSampleRate);
  const source = offline.createBufferSource();
  source.buffer = decoded;
  source.connect(offline.destination);
  source.start();
  const rendered = await offline.startRendering();

  return new Blob([encodeWav(rendered.getChannelData(0), targetSampleRate)], {
    type: WAV_MIME_TYPE,
  });
}

/** encodeWav 给一段 [-1, 1] 的浮点采样加上 44 字节 WAV 头，输出 PCM16LE。 */
function encodeWav(samples: Float32Array, sampleRate: number): ArrayBuffer {
  const bytesPerSample = 2;
  const channels = 1;
  const blockAlign = channels * bytesPerSample;
  const dataBytes = samples.length * bytesPerSample;
  const buffer = new ArrayBuffer(44 + dataBytes);
  const view = new DataView(buffer);

  writeAscii(view, 0, "RIFF");
  view.setUint32(4, 36 + dataBytes, true); // 除 RIFF 标识与本字段外的总长度
  writeAscii(view, 8, "WAVE");

  writeAscii(view, 12, "fmt ");
  view.setUint32(16, 16, true); // fmt 块长度，PCM 固定 16
  view.setUint16(20, 1, true); // 编码格式，1 = 未压缩 PCM
  view.setUint16(22, channels, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * blockAlign, true); // 每秒字节数
  view.setUint16(32, blockAlign, true);
  view.setUint16(34, 8 * bytesPerSample, true); // 位深

  writeAscii(view, 36, "data");
  view.setUint32(40, dataBytes, true);

  let offset = 44;
  for (let i = 0; i < samples.length; i += 1) {
    // 先夹紧再定标：超出 [-1, 1] 的采样点直接转 Int16 会溢出成反相的爆音。
    const clamped = Math.max(-1, Math.min(1, samples[i]));
    view.setInt16(offset, clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff, true);
    offset += bytesPerSample;
  }
  return buffer;
}

function writeAscii(view: DataView, offset: number, text: string): void {
  for (let i = 0; i < text.length; i += 1) {
    view.setUint8(offset + i, text.charCodeAt(i));
  }
}
