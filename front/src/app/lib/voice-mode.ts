export type VoiceInputMode = "realtime" | "upload";

export function selectVoiceInputMode(
  realtimeVoiceEnabled: boolean,
  fileVoiceEnabled: boolean,
): VoiceInputMode | null {
  if (realtimeVoiceEnabled) return "realtime";
  if (fileVoiceEnabled) return "upload";
  return null;
}
