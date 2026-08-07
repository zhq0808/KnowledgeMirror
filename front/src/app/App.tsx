import {
  useState,
  useRef,
  useEffect,
  useLayoutEffect,
  useCallback,
  useMemo,
} from "react";
import { AnimatePresence, motion } from "motion/react";
import { BrainCircuit } from "lucide-react";
import { StatusTags } from "./components/StatusTags";
import { InputDock } from "./components/InputDock";
import { UserMessage } from "./components/UserMessage";
import { AIMessage } from "./components/AIMessage";
import { AppHeader } from "./components/AppHeader";
import { SessionDrawer } from "./components/SessionDrawer";
import { Dashboard } from "./components/Dashboard";
import { BottomNavigation, type AppView } from "./components/BottomNavigation";
import { KnowledgeBasePage } from "./components/KnowledgeBasePage";
import { ProfileDashboard } from "./components/ProfileDashboard";
import { PracticeStatusBar } from "./components/PracticeStatusBar";
import {
  getFeynmanPracticeState,
  type FeynmanPracticeState,
} from "./api/feynman";
import {
  createOrResumeGuest,
  ensureSessionID,
  sendChatStream,
  listSessions,
  listSessionMessages,
  createNewSession,
  getActiveSessionID,
  rememberSessionID,
  type SessionListItem,
  type SessionMessage,
  type RetrievalSources,
  ChatStreamError,
} from "./api/chat";
import { AuthPage } from "./pages/AuthPage";
import {
  getApplicationCapabilities,
  type ApplicationCapabilities,
} from "./api/capabilities";
import {
  getCoachGaps,
  getCoachProgress,
  getCoachToday,
  type CoachGap,
  type CoachProgress,
  type CoachTask,
  type CoachToday,
} from "./api/coach";
import {
  addLocalDays,
  buildCoachStatusTags,
  localDateKey,
} from "./lib/coach-view-model";

interface Message {
  id: string;
  type: "user" | "ai";
  content: string;
  // time 为气泡下方展示的时间(HH:MM)。历史消息用后端 created_at；实时消息渲染时按需生成。
  time?: string;
  // failed 标记该助手气泡对应的发送失败，需展示重试入口。
  failed?: boolean;
  // retry 保存失败发送的原始负载：重试时复用同一 client_message_id 命中后端幂等，
  // 避免重复计费/重复落库；只有全新发送才生成新的 UUID。
  retry?: {
    sessionID: string;
    clientMessageID: string;
    text: string;
    options: {
      voiceCaptureID?: string;
      coachTaskID?: string;
      localDate?: string;
    };
    retryable: boolean;
  };
  retrieval?: RetrievalSources;
}

const WELCOME_MESSAGE: Message = {
  id: "welcome",
  type: "ai",
  content: "准备好开始今天的面试训练了吗？",
};

// formatClock 把时间格式化成 HH:MM（与旧版一致：zh-CN 24 小时制），用于气泡下方的时间戳。
function formatClock(date: Date): string {
  return date.toLocaleTimeString("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
  });
}

// mapBackendMessages 把后端历史消息映射成聊天区气泡；无历史时回退到欢迎语。
function mapBackendMessages(items: SessionMessage[]): Message[] {
  if (items.length === 0) return [WELCOME_MESSAGE];
  return items.map((item) => ({
    id: `srv-${item.message_id}`,
    type: item.role === "assistant" ? "ai" : "user",
    content: item.content,
    retrieval: item.retrieval,
    time: formatClock(new Date(item.created_at)),
  }));
}

function InterviewWorkspace() {
  const [messages, setMessages] = useState<Message[]>([
    {
      id: "welcome",
      type: "ai",
      content: "准备好开始今天的面试训练了吗？",
    },
  ]);

  const [activeSessionID, setActiveSessionID] = useState<string | null>(null);
  const [sessions, setSessions] = useState<SessionListItem[]>([]);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [sessionsError, setSessionsError] = useState<string | null>(null);
  const [sessionDrawerOpen, setSessionDrawerOpen] = useState(false);
  const [activeView, setActiveView] = useState<AppView>("practice");
  // 费曼练习不再是一个前端模式，而是服务端维护的会话状态：
  // 前端只负责展示，绝不自己推断“现在在不在练习”，否则刷新后两边就对不上了。
  const [practiceState, setPracticeState] = useState<FeynmanPracticeState | null>(null);
  const [profileOpen, setProfileOpen] = useState(false);
  const [isSending, setIsSending] = useState(false);
  const [transitionBusy, setTransitionBusy] = useState(false);
  const [capabilities, setCapabilities] = useState<ApplicationCapabilities | null>(null);
  const [capabilityLoading, setCapabilityLoading] = useState(true);
  const [capabilityError, setCapabilityError] = useState<string | null>(null);
  const [coachToday, setCoachToday] = useState<CoachToday | null>(null);
  const [coachProgress, setCoachProgress] = useState<CoachProgress | null>(null);
  const [coachGaps, setCoachGaps] = useState<CoachGap[]>([]);
  const [coachLoading, setCoachLoading] = useState(false);
  const [coachError, setCoachError] = useState<string | null>(null);
  const [gapsLoaded, setGapsLoaded] = useState(false);
  const [gapsLoading, setGapsLoading] = useState(false);
  const [gapsError, setGapsError] = useState<string | null>(null);
  const [launchingTaskID, setLaunchingTaskID] = useState<string | null>(null);
  const [inputDockHeight, setInputDockHeight] = useState(176);
  const tags = useMemo(
    () => buildCoachStatusTags(coachProgress, coachToday),
    [coachProgress, coachToday],
  );
  const scrollRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  const activeSessionIDRef = useRef<string | null>(null);
  const sessionLoadGenerationRef = useRef(0);
  const coachRefreshGenerationRef = useRef(0);
  const transitionBusyRef = useRef(false);

  const updateActiveSessionID = useCallback((sessionID: string | null) => {
    activeSessionIDRef.current = sessionID;
    setActiveSessionID(sessionID);
    if (sessionID) rememberSessionID(sessionID);
  }, []);
  // resolveTime 为每条消息提供稳定的时间戳：历史消息用自带 time，实时消息按 id 缓存首次渲染时刻。
  const timeCacheRef = useRef<Map<string, string>>(new Map());
  const resolveTime = (message: Message): string => {
    if (message.time) return message.time;
    const cache = timeCacheRef.current;
    let cached = cache.get(message.id);
    if (!cached) {
      cached = formatClock(new Date());
      cache.set(message.id, cached);
    }
    return cached;
  };

  // refreshSessions 拉取会话列表；返回最新列表供引导逻辑使用。
  const refreshSessions = async (): Promise<SessionListItem[]> => {
    setSessionsLoading(true);
    setSessionsError(null);
    try {
      const list = await listSessions();
      setSessions(list);
      return list;
    } catch {
      setSessionsError("会话列表加载失败");
      return [];
    } finally {
      setSessionsLoading(false);
    }
  };

  const isCurrentSessionLoad = (sessionID: string, generation: number) =>
    generation === sessionLoadGenerationRef.current &&
    activeSessionIDRef.current === sessionID;

  // loadSessionMessages 用后端历史消息替换聊天区；过期切换请求不得覆盖新会话。
  const loadSessionMessages = async (sessionID: string, generation: number) => {
    try {
      const items = await listSessionMessages(sessionID);
      if (isCurrentSessionLoad(sessionID, generation)) {
        setMessages(mapBackendMessages(items));
      }
    } catch {
      if (isCurrentSessionLoad(sessionID, generation)) {
        setMessages([WELCOME_MESSAGE]);
      }
    }
  };

  // refreshPracticeState 以服务端为准恢复练习状态条；过期请求不得覆盖新会话。
  const refreshPracticeState = async (sessionID: string, generation: number) => {
    try {
      const state = await getFeynmanPracticeState(sessionID);
      if (isCurrentSessionLoad(sessionID, generation)) setPracticeState(state);
    } catch {
      if (isCurrentSessionLoad(sessionID, generation)) setPracticeState(null);
    }
  };

  const startSessionTransition = (): number | null => {
    if (transitionBusyRef.current) return null;
    transitionBusyRef.current = true;
    setTransitionBusy(true);
    return ++sessionLoadGenerationRef.current;
  };

  const finishSessionTransition = (generation: number) => {
    if (generation !== sessionLoadGenerationRef.current) return;
    transitionBusyRef.current = false;
    setTransitionBusy(false);
  };

  const loadActiveSession = async (
    sessionID: string,
    generation: number,
    load = true,
  ) => {
    updateActiveSessionID(sessionID);
    setPracticeState(null);
    if (!load) {
      setMessages([WELCOME_MESSAGE]);
      return isCurrentSessionLoad(sessionID, generation);
    }
    await Promise.all([
      loadSessionMessages(sessionID, generation),
      refreshPracticeState(sessionID, generation),
    ]);
    return isCurrentSessionLoad(sessionID, generation);
  };

  const switchSession = async (sessionID: string, options: { load?: boolean } = {}) => {
    const generation = startSessionTransition();
    if (generation == null) return false;
    try {
      return await loadActiveSession(sessionID, generation, options.load !== false);
    } finally {
      finishSessionTransition(generation);
    }
  };

  const refreshCoachData = useCallback(
    async (includeGaps = false) => {
      if (capabilities?.coach !== true) return;

      const generation = ++coachRefreshGenerationRef.current;
      setCoachLoading(true);
      setCoachError(null);
      if (includeGaps) {
        setGapsLoading(true);
        setGapsError(null);
      }
      const now = new Date();
      const requests: Promise<unknown>[] = [
        getCoachToday(now),
        getCoachProgress(addLocalDays(now, -89), now),
      ];
      if (includeGaps) requests.push(getCoachGaps("open", 50));

      const [todayResult, progressResult, gapsResult] = await Promise.allSettled(requests);
      if (generation !== coachRefreshGenerationRef.current) return;

      const errors: string[] = [];
      if (todayResult.status === "fulfilled") {
        setCoachToday(todayResult.value as CoachToday);
      } else {
        errors.push(todayResult.reason instanceof Error ? todayResult.reason.message : "今日计划加载失败");
      }
      if (progressResult.status === "fulfilled") {
        setCoachProgress(progressResult.value as CoachProgress);
      } else {
        errors.push(progressResult.reason instanceof Error ? progressResult.reason.message : "教练进度加载失败");
      }
      if (includeGaps && gapsResult) {
        if (gapsResult.status === "fulfilled") {
          setCoachGaps(gapsResult.value as CoachGap[]);
          setGapsLoaded(true);
          setGapsError(null);
        } else {
          setGapsError(
            gapsResult.reason instanceof Error ? gapsResult.reason.message : "薄弱点加载失败",
          );
        }
      }
      setCoachError(errors.length ? errors.join("；") : null);
      setCoachLoading(false);
      if (includeGaps) setGapsLoading(false);
    },
    [capabilities?.coach],
  );

  // 身份建立后的首轮能力探测会触发真实 Coach 数据加载；进入计划或档案时重新拉取。
  useEffect(() => {
    if (capabilities?.coach !== true) return;
    void refreshCoachData(profileOpen || activeView === "plan");
  }, [activeView, profileOpen, capabilities?.coach, refreshCoachData]);

  const retryCapabilities = useCallback(async () => {
    setCapabilityLoading(true);
    setCapabilityError(null);
    try {
      setCapabilities(await getApplicationCapabilities());
    } catch (error) {
      setCapabilities(null);
      setCapabilityError(error instanceof Error ? error.message : "服务能力加载失败");
    } finally {
      setCapabilityLoading(false);
    }
  }, []);

  // 启动引导：拉列表 → 校验 localStorage → 选最近会话 → 无会话再创建 → 载入消息。
  useEffect(() => {
    let cancelled = false;
    void retryCapabilities();
    (async () => {
      const list = await refreshSessions();
      if (cancelled) return;

      const stored = getActiveSessionID();
      let active: string | null =
        stored && list.some((s) => s.session_id === stored)
          ? stored
          : list[0]?.session_id ?? null;

      if (!active) {
        try {
          active = await createNewSession();
          if (!cancelled) await refreshSessions();
        } catch {
          active = null;
        }
      }

      if (cancelled || !active) return;
      await switchSession(active);
    })();

    return () => {
      cancelled = true;
    };
    // 仅在挂载时引导一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleSelectSession = async (sessionID: string) => {
    if (isSending || launchingTaskID || transitionBusyRef.current) return;
    setSessionDrawerOpen(false);
    if (sessionID === activeSessionIDRef.current) return;
    await switchSession(sessionID);
  };

  const handleCreateSession = async (): Promise<string | null> => {
    if (isSending || transitionBusyRef.current) return null;
    const generation = startSessionTransition();
    if (generation == null) return null;
    try {
      const sessionID = await createNewSession();
      if (!(await loadActiveSession(sessionID, generation, false))) return null;
      setSessionDrawerOpen(false);
      await refreshSessions();
      return sessionID;
    } catch {
      setSessionsError("创建会话失败");
      return null;
    } finally {
      finishSessionTransition(generation);
    }
  };

  // handleRenameSession 会话重命名。TODO(后端): 接入 PATCH /api/v1/sessions/:id
  // 后改为调用后端并以返回结果为准；当前仅前端本地生效，刷新后会被后端列表覆盖。
  const handleRenameSession = (sessionID: string, title: string) => {
    if (isSending || transitionBusyRef.current) return;
    setSessions((prev) =>
      prev.map((s) => (s.session_id === sessionID ? { ...s, title } : s))
    );
  };

  // handleDeleteSession 删除会话。TODO(后端): 接入 DELETE /api/v1/sessions/:id
  // 后改为调用后端软删除；当前仅前端本地移除，刷新后会被后端列表覆盖。
  const handleDeleteSession = async (sessionID: string) => {
    if (isSending || transitionBusyRef.current) return;
    const remaining = sessions.filter((s) => s.session_id !== sessionID);
    setSessions(remaining);

    if (sessionID === activeSessionIDRef.current) {
      const next = remaining[0]?.session_id;
      if (next) {
        await switchSession(next);
      } else {
        const replacement = await createNewSession();
        await switchSession(replacement, { load: false });
        await refreshSessions();
      }
    }
  };


  // Auto-scroll to bottom on new messages
  useLayoutEffect(() => {
    if (scrollRef.current) scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
  }, [messages, inputDockHeight]);

  // streamAssistantReply 向后端发起一次流式对话，并把增量原地写入 targetId 对应的气泡。
  // clientMessageID 由调用方决定：首次发送用新 UUID，重试时复用原 UUID 命中后端幂等。
  const streamAssistantReply = async (
    sessionID: string,
    text: string,
    clientMessageID: string,
    targetId: string,
    options: {
      voiceCaptureID?: string;
      coachTaskID?: string;
      localDate?: string;
    } = {},
  ) => {
    const controller = new AbortController();
    abortRef.current = controller;
    let acc = "";
    setIsSending(true);
    try {
      await sendChatStream(
        sessionID,
        clientMessageID,
        text,
        (delta) => {
          acc += delta;
          if (activeSessionIDRef.current !== sessionID) return;
          setMessages((prev) =>
            prev.map((m) =>
              m.id === targetId
                ? { ...m, content: acc, failed: false, retry: undefined }
                : m
            )
          );
        },
        (retrieval) => {
          if (activeSessionIDRef.current !== sessionID) return;
          setMessages((prev) =>
            prev.map((message) =>
              message.id === targetId ? { ...message, retrieval } : message
            )
          );
        },
        {
          signal: controller.signal,
          onPracticeState: (state) => {
            if (activeSessionIDRef.current === sessionID) setPracticeState(state);
          },
          voiceCaptureID: options.voiceCaptureID,
          coachTaskID: options.coachTaskID,
          localDate: options.localDate,
        },
      );
      void refreshSessions();
    } catch (error) {
      if (controller.signal.aborted) {
        if (activeSessionIDRef.current === sessionID) {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === targetId
                ? { ...m, content: acc || "（已停止回答）" }
                : m
            )
          );
        }
        void refreshSessions();
      } else if (activeSessionIDRef.current === sessionID) {
        const streamError = error instanceof ChatStreamError ? error : null;
        const retryable = streamError?.retryable ?? true;
        setMessages((prev) =>
          prev.map((m) =>
            m.id === targetId
              ? {
                  ...m,
                  content: streamError?.message || "抱歉，暂时没能连上知镜，请稍后再试。",
                  failed: true,
                  retry: {
                    sessionID,
                    clientMessageID,
                    text,
                    options: { ...options },
                    retryable,
                  },
                }
              : m
          )
        );
      }
    } finally {
      if (abortRef.current === controller) {
        abortRef.current = null;
        setIsSending(false);
      }
      if (capabilities?.coach === true && options.coachTaskID) {
        void refreshCoachData(profileOpen || activeView === "plan");
        const generation = sessionLoadGenerationRef.current;
        void refreshPracticeState(sessionID, generation);
      }
    }
  };

  const handleSendMessage = async (text: string, voiceCaptureID?: string) => {
    if (isSending || transitionBusyRef.current) return;
    let sessionID = activeSessionIDRef.current;
    if (!sessionID) {
      sessionID = await ensureSessionID();
      if (!(await switchSession(sessionID, { load: false }))) return;
    }

    const userMessage: Message = {
      id: Date.now().toString(),
      type: "user",
      content: text,
    };
    setMessages((prev) => [...prev, userMessage]);

    // 其余自由对话走真实 chat 接口。先插入空占位气泡（显示三点动画），返回后原地替换。
    const typingId = Date.now().toString() + "-typing";
    setMessages((prev) => [
      ...prev,
      { id: typingId, type: "ai", content: "" },
    ]);

    // 活动 Coach 的答案和控制 turn 自动携带任务 ID；本地日只随 Coach turn 发送。
    const coachTaskID = practiceState?.coach_task_id || undefined;
    await streamAssistantReply(sessionID, text, crypto.randomUUID(), typingId, {
      voiceCaptureID,
      coachTaskID,
      localDate: coachTaskID ? localDateKey(new Date()) : undefined,
    });
  };

  // handleRetryMessage 重试失败的助手回复：复用原 client_message_id，让后端幂等去重
  // （已完成则回放、进行中则拒绝、失败/过期才真正重跑），不会重复扣费或重复落库。
  const handleRetryMessage = async (messageId: string) => {
    if (isSending || transitionBusyRef.current) return;
    const target = messages.find((m) => m.id === messageId);
    if (!target?.retry?.retryable) return;
    const { sessionID, clientMessageID, text, options } = target.retry;
    if (activeSessionIDRef.current !== sessionID) {
      const switched = await switchSession(sessionID);
      if (!switched) return;
      setMessages((prev) => [
        ...prev,
        { ...target, content: "", failed: false, retry: undefined },
      ]);
    } else {
      setMessages((prev) =>
        prev.map((m) =>
          m.id === messageId
            ? { ...m, content: "", failed: false, retry: undefined }
            : m
        )
      );
    }
    await streamAssistantReply(sessionID, text, clientMessageID, messageId, options);
  };

  const handleLaunchCoachTask = async (task: CoachTask) => {
    if (isSending || launchingTaskID || transitionBusyRef.current) return;
    setLaunchingTaskID(task.coach_task_id);
    setActiveView("practice");
    setProfileOpen(false);
    try {
      let sessionID: string | null = null;
      if (task.status === "pending") {
        const practiceActive = practiceState != null && practiceState.state !== "idle";
        if (practiceActive) {
          sessionID = await handleCreateSession();
        } else {
          sessionID = activeSessionIDRef.current ?? (await ensureSessionID());
          if (sessionID !== activeSessionIDRef.current) {
            if (!(await switchSession(sessionID, { load: false }))) return;
          }
        }
      } else {
        if (!task.session_id) {
          setCoachError("该教练任务缺少原会话，无法安全继续");
          return;
        }
        sessionID = task.session_id;
        if (sessionID !== activeSessionIDRef.current) {
          if (!(await switchSession(sessionID))) return;
        }
      }
      if (!sessionID) return;

      const text = task.status === "pending" ? "开始今日教练任务" : "继续";
      const userID = `${Date.now()}-coach-start`;
      const typingID = `${userID}-typing`;
      setMessages((prev) => [
        ...prev,
        { id: userID, type: "user", content: text },
        { id: typingID, type: "ai", content: "" },
      ]);
      await streamAssistantReply(sessionID, text, crypto.randomUUID(), typingID, {
        coachTaskID: task.coach_task_id,
      });
    } finally {
      setLaunchingTaskID(null);
    }
  };

  const openKnowledgeFromCoach = () => {
    setProfileOpen(false);
    setActiveView("knowledge");
  };

  // handleStop 用户主动中止正在进行的流式回复。
  const handleStop = () => {
    abortRef.current?.abort();
  };

  const handleSelectPracticePrompt = (prompt: { emoji: string; label: string }) => {
    // “费曼学习”只是一个显式入口：它发的是一句开始练习的对话意图，
    // 不切页面、不开表单。后续的提问→回答→分析→下一题全部在同一个聊天框里发生。
    if (prompt.label === "费曼学习") {
      void handleSendMessage("我想开始费曼学习练习");
      return;
    }
    void handleSendMessage(`${prompt.emoji} ${prompt.label}`);
  };

  // 对话是否已开始：只剩欢迎语时视为未开始，此时欢迎语居中显示。
  const conversationStarted = messages.some((m) => m.id !== "welcome");

  return (
    <div className="size-full flex flex-col overflow-hidden bg-background relative">
      {profileOpen ? (
        <ProfileDashboard
          onBack={() => setProfileOpen(false)}
          coachEnabled={capabilities?.coach === true}
          capabilityLoading={capabilityLoading}
          capabilityError={capabilityError}
          progress={coachProgress}
          gaps={coachGaps}
          gapsLoaded={gapsLoaded}
          gapsLoading={gapsLoading}
          gapsError={gapsError}
          loading={coachLoading}
          error={coachError}
          globalBusy={isSending || transitionBusy || Boolean(launchingTaskID)}
          onRetry={() => {
            if (capabilityError) void retryCapabilities();
            else void refreshCoachData(true);
          }}
        />
      ) : activeView === "practice" ? (
        <>
          <AppHeader
            onOpenSessions={() => setSessionDrawerOpen(true)}
            onOpenProfile={() => setProfileOpen(true)}
          />

          <StatusTags tags={tags} />

          <PracticeStatusBar state={practiceState} />

          <div ref={scrollRef} className="flex-1 overflow-y-auto">
            {!conversationStarted ? (
              <div
                className="min-h-full flex flex-col items-center justify-center px-6 text-center"
                style={{ paddingBottom: inputDockHeight + 104 }}
              >
            <motion.div
              initial={{ opacity: 0, y: 10 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.4, ease: "easeOut" }}
              className="flex flex-col items-center"
            >
              <div className="w-16 h-16 rounded-3xl bg-primary flex items-center justify-center mb-5 shadow-sm">
                <BrainCircuit size={31} className="text-white" />
              </div>
              <h2 className="text-2xl font-semibold text-foreground">
                今天想练哪一项？
              </h2>
              <p className="mt-2 max-w-xs text-sm leading-relaxed text-muted-foreground">
                用一次主动输出，检验你是否真的能讲清楚。
              </p>
            </motion.div>
              </div>
            ) : (
        <div
          className="flex flex-col pt-2"
          style={{ paddingBottom: inputDockHeight + 104 }}
        >
          <AnimatePresence>
            {messages.map((message) => {
              switch (message.type) {
                case "user":
                  return (
                    <UserMessage
                      key={message.id}
                      message={message.content}
                      time={resolveTime(message)}
                    />
                  );
                case "ai":
                  return (
                    <AIMessage
                      key={message.id}
                      message={message.content}
                      time={resolveTime(message)}
                      failed={message.failed}
                      retrieval={message.retrieval}
                      speechEnabled={capabilities?.speech === true}
                      onRetry={
                        message.failed && message.retry?.retryable
                          ? () => handleRetryMessage(message.id)
                          : undefined
                      }
                      retryDisabled={
                        !message.retry?.retryable || isSending || transitionBusy || Boolean(launchingTaskID)
                      }
                    />
                  );
                default:
                  return null;
              }
            })}
          </AnimatePresence>

        </div>
            )}
          </div>

          <InputDock
            onSendMessage={(text, voiceCaptureID) => {
              void handleSendMessage(text, voiceCaptureID);
            }}
            sessionID={activeSessionID}
            onSelectPrompt={handleSelectPracticePrompt}
            realtimeVoiceEnabled={capabilities?.realtime_voice === true}
            fileVoiceEnabled={capabilities?.file_voice === true}
            voiceCapabilityLoading={capabilityLoading}
            voiceCapabilityError={capabilityError}
            onRetryVoiceCapabilities={() => void retryCapabilities()}
            isResponding={isSending}
            disabled={transitionBusy || Boolean(launchingTaskID)}
            onStop={handleStop}
            onHeightChange={setInputDockHeight}
          />

          <SessionDrawer
            open={sessionDrawerOpen}
            sessions={sessions}
            activeSessionID={activeSessionID}
            loading={sessionsLoading}
            error={sessionsError}
            busy={isSending || transitionBusy || Boolean(launchingTaskID)}
            onClose={() => setSessionDrawerOpen(false)}
            onSelect={handleSelectSession}
            onCreate={handleCreateSession}
            onRetry={refreshSessions}
            onRename={handleRenameSession}
            onDelete={handleDeleteSession}
          />
        </>
      ) : activeView === "plan" ? (
        <Dashboard
          mode="page"
          onOpenProfile={() => setProfileOpen(true)}
          coachEnabled={capabilities?.coach === true}
          capabilityLoading={capabilityLoading}
          capabilityError={capabilityError}
          today={coachToday}
          progress={coachProgress}
          gaps={coachGaps}
          gapsLoaded={gapsLoaded}
          gapsLoading={gapsLoading}
          gapsError={gapsError}
          loading={coachLoading}
          error={coachError}
          globalBusy={isSending || transitionBusy || Boolean(launchingTaskID)}
          launchingTaskID={launchingTaskID}
          onRetry={() => {
            if (capabilityError) void retryCapabilities();
            else void refreshCoachData(true);
          }}
          onLaunchTask={(task) => void handleLaunchCoachTask(task)}
          onEmptyStateAction={openKnowledgeFromCoach}
        />
      ) : (
        <KnowledgeBasePage onOpenProfile={() => setProfileOpen(true)} />
      )}

      {!profileOpen && (
        <BottomNavigation
          activeView={activeView}
          onChange={(view) => {
            setSessionDrawerOpen(false);
            setActiveView(view);
          }}
        />
      )}
    </div>
  );
}

export default function App() {
  const guestStartedKey = "interview_agent_guest_started";
  const [authState, setAuthState] = useState<"auth" | "restoring" | "guest">(() =>
    localStorage.getItem(guestStartedKey) === "1" ? "restoring" : "auth"
  );

  useEffect(() => {
    if (authState !== "restoring") return;

    let cancelled = false;
    createOrResumeGuest()
      .then(() => ensureSessionID())
      .then(() => {
        if (!cancelled) setAuthState("guest");
      })
      .catch(() => {
        localStorage.removeItem(guestStartedKey);
        if (!cancelled) setAuthState("auth");
      });

    return () => {
      cancelled = true;
    };
  }, [authState]);

  const continueAsGuest = async () => {
    await createOrResumeGuest();
    await ensureSessionID();
    localStorage.setItem(guestStartedKey, "1");
    setAuthState("guest");
  };

  if (authState === "guest") {
    return <InterviewWorkspace />;
  }

  if (authState === "restoring") {
    return (
      <main className="flex min-h-dvh items-center justify-center bg-[#F4F2ED] text-[#2E5E3E]">
        <p className="text-sm font-medium">正在恢复访客身份...</p>
      </main>
    );
  }

  return <AuthPage onContinueAsGuest={continueAsGuest} />;
}
