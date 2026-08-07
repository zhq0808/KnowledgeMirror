interface APIResponse<T> {
  code: number;
  message: string;
  data: T;
}

export interface ApplicationCapabilities {
  realtime_voice: boolean;
  speech: boolean;
  coach: boolean;
}

export async function getApplicationCapabilities(): Promise<ApplicationCapabilities> {
  const response = await fetch("/api/v1/capabilities", {
    credentials: "include",
  });
  if (!response.ok) throw new Error("服务能力加载失败");

  const body = (await response.json()) as APIResponse<ApplicationCapabilities>;
  if (!body.data) throw new Error(body.message || "服务能力加载失败");
  return body.data;
}