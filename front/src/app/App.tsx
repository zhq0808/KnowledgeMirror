import { useState, useRef, useEffect } from "react";
import { AnimatePresence, motion } from "motion/react";
import { BrainCircuit } from "lucide-react";
import { StatusTags, StatusTagDef } from "./components/StatusTags";
import { MealSuggestionCard } from "./components/MealSuggestionCard";
import { ActionCard } from "./components/ActionCard";
import { WarningCard } from "./components/WarningCard";
import { ConfirmationCard } from "./components/ConfirmationCard";
import { MorningGreetingCard } from "./components/MorningGreetingCard";
import { InputDock } from "./components/InputDock";
import { UserMessage } from "./components/UserMessage";
import { AIMessage } from "./components/AIMessage";
import { MealCard } from "./components/MealCard";
import { AppHeader } from "./components/AppHeader";
import { SessionDrawer } from "./components/SessionDrawer";
import { Dashboard } from "./components/Dashboard";
import { BottomNavigation, type AppView } from "./components/BottomNavigation";
import { KnowledgeBasePage } from "./components/KnowledgeBasePage";
import { ProfileDashboard } from "./components/ProfileDashboard";
import {
  createOrResumeGuest,
  ensureSessionID,
  sendChatStream,
  listSessions,
  listSessionMessages,
  createNewSession,
  getActiveSessionID,
  rememberSessionID,
  MODELS,
  getSelectedModelID,
  rememberModelID,
  type SessionListItem,
  type SessionMessage,
  type RetrievalSources,
} from "./api/chat";
import { AuthPage } from "./pages/AuthPage";

interface Message {
  id: string;
  type:
    | "user"
    | "ai"
    | "meal-card"
    | "meal-suggestion"
    | "action-card"
    | "warning"
    | "confirmation"
    | "morning-greeting";
  content: any;
  // time 为气泡下方展示的时间(HH:MM)。历史消息用后端 created_at；实时消息渲染时按需生成。
  time?: string;
  // failed 标记该助手气泡对应的发送失败，需展示重试入口。
  failed?: boolean;
  // retry 保存失败发送的原始负载：重试时复用同一 client_message_id 命中后端幂等，
  // 避免重复计费/重复落库；只有全新发送才生成新的 UUID。
  retry?: { clientMessageID: string; text: string };
  retrieval?: RetrievalSources;
}

interface ActionItem {
  id: string;
  text: string;
  completed: boolean;
}

const initialActions: ActionItem[] = [
  { id: "1", text: "餐前测血糖", completed: true },
  { id: "2", text: "饮用 500ml 温水", completed: false },
  { id: "3", text: "完成 20 分钟散步", completed: false },
];

const INITIAL_TAGS: StatusTagDef[] = [
  {
    id: "output-streak",
    emoji: "🔥",
    label: "连续输出 3 天",
    color: "bg-[#FFF5E6] text-[#A67C52]",
    state: "active",
    sparklineData: [
      { v: 4 },
      { v: 5 },
      { v: 6 },
      { v: 6 },
      { v: 7 },
      { v: 8 },
      { v: 8 },
    ],
    summary: "连续 3 天完成主动输出，本周已产生 5 条练习证据。",
  },
  {
    id: "review",
    emoji: "📌",
    label: "2 项待回顾",
    color: "bg-[#E9EEF8] text-[#526A91]",
    state: "active",
    expandable: false,
    sparklineData: [
      { v: 3 },
      { v: 4 },
      { v: 5 },
      { v: 5 },
      { v: 4 },
      { v: 3 },
      { v: 3 },
    ],
    summary: "Kafka 消息积压与 Go GC 将在今天进入间隔复习。",
  },
];

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
  const [tags, setTags] = useState<StatusTagDef[]>(INITIAL_TAGS);
  const [actions, setActions] = useState<ActionItem[]>(initialActions);
  const [messages, setMessages] = useState<Message[]>([
    {
      id: "welcome",
      type: "ai",
      content: "准备好开始今天的面试训练了吗？",
    },
  ]);

  const [pendingConfirmation, setPendingConfirmation] =
    useState<Message | null>(null);
  const [activeSessionID, setActiveSessionID] = useState<string | null>(null);
  const [sessions, setSessions] = useState<SessionListItem[]>([]);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [sessionsError, setSessionsError] = useState<string | null>(null);
  const [sessionDrawerOpen, setSessionDrawerOpen] = useState(false);
  const [activeView, setActiveView] = useState<AppView>("practice");
  const [profileOpen, setProfileOpen] = useState(false);
  const [isSending, setIsSending] = useState(false);
  const [selectedModelID, setSelectedModelID] = useState<string>(() =>
    getSelectedModelID()
  );
  const scrollRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);
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

  // loadSessionMessages 用后端历史消息替换聊天区；无历史时回退到欢迎语。
  const loadSessionMessages = async (sessionID: string) => {
    try {
      const items = await listSessionMessages(sessionID);
      setMessages(mapBackendMessages(items));
    } catch {
      setMessages([WELCOME_MESSAGE]);
    }
  };

  // 启动引导：拉列表 → 校验 localStorage → 选最近会话 → 无会话再创建 → 载入消息。
  useEffect(() => {
    let cancelled = false;
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
      setActiveSessionID(active);
      rememberSessionID(active);
      await loadSessionMessages(active);
    })();

    return () => {
      cancelled = true;
    };
    // 仅在挂载时引导一次。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleSelectSession = async (sessionID: string) => {
    if (isSending) return;
    setSessionDrawerOpen(false);
    if (sessionID === activeSessionID) return;
    setActiveSessionID(sessionID);
    rememberSessionID(sessionID);
    setPendingConfirmation(null);
    await loadSessionMessages(sessionID);
  };

  const handleCreateSession = async () => {
    if (isSending) return;
    try {
      const sessionID = await createNewSession();
      setActiveSessionID(sessionID);
      rememberSessionID(sessionID);
      setPendingConfirmation(null);
      setMessages([WELCOME_MESSAGE]);
      setSessionDrawerOpen(false);
      await refreshSessions();
    } catch {
      setSessionsError("创建会话失败");
    }
  };

  // handleRenameSession 会话重命名。TODO(后端): 接入 PATCH /api/v1/sessions/:id
  // 后改为调用后端并以返回结果为准；当前仅前端本地生效，刷新后会被后端列表覆盖。
  const handleRenameSession = (sessionID: string, title: string) => {
    setSessions((prev) =>
      prev.map((s) => (s.session_id === sessionID ? { ...s, title } : s))
    );
  };

  // handleDeleteSession 删除会话。TODO(后端): 接入 DELETE /api/v1/sessions/:id
  // 后改为调用后端软删除；当前仅前端本地移除，刷新后会被后端列表覆盖。
  const handleDeleteSession = (sessionID: string) => {
    if (isSending) return;
    const remaining = sessions.filter((s) => s.session_id !== sessionID);
    setSessions(remaining);

    if (sessionID === activeSessionID) {
      const next = remaining[0]?.session_id ?? null;
      setActiveSessionID(next);
      setPendingConfirmation(null);
      if (next) {
        rememberSessionID(next);
        void loadSessionMessages(next);
      } else {
        setMessages([WELCOME_MESSAGE]);
      }
    }
  };


  // Auto-scroll to bottom on new messages
  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTo({
        top: scrollRef.current.scrollHeight,
        behavior: "smooth",
      });
    }
  }, [messages, pendingConfirmation]);

  // Helpers
  const latestMealId = [...messages]
    .reverse()
    .find((m) => m.type === "meal-suggestion")?.id;
  const latestActionId = [...messages]
    .reverse()
    .find((m) => m.type === "action-card")?.id;

  const dismissMorningGreeting = () => {
    setMessages((prev) =>
      prev.filter((m) => m.id !== "morning-greeting")
    );
  };

  const handleMorningReply = (reply: "recovered" | "still-tired") => {
    dismissMorningGreeting();

    if (reply === "recovered") {
      setTags((prev) =>
        prev.map((t) =>
          t.id === "energy" ? { ...t, state: "dismissed" } : t
        )
      );
      setTimeout(() => {
        setMessages((prev) => [
          ...prev,
          {
            id: Date.now().toString(),
            type: "ai",
            content:
              "太好了！精气神回来了 ✨ 今天继续保持，身体在慢慢向好的方向走。",
          },
        ]);
      }, 400);
    } else {
      setTags((prev) =>
        prev.map((t) =>
          t.id === "energy" ? { ...t, state: "active" } : t
        )
      );
      setTimeout(() => {
        setMessages((prev) => [
          ...prev,
          {
            id: Date.now().toString(),
            type: "ai",
            content:
              "没关系，身体需要休息的信号值得被认真对待 🫧 今天可以减少剧烈运动，多喝温水。",
          },
        ]);
      }, 400);
    }
  };

  // streamAssistantReply 向后端发起一次流式对话，并把增量原地写入 targetId 对应的气泡。
  // clientMessageID 由调用方决定：首次发送用新 UUID，重试时复用原 UUID 命中后端幂等。
  const streamAssistantReply = async (
    sessionID: string,
    text: string,
    clientMessageID: string,
    targetId: string
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
          setMessages((prev) =>
            prev.map((m) =>
              m.id === targetId
                ? { ...m, content: acc, failed: false, retry: undefined }
                : m
            )
          );
        },
        (retrieval) => {
          setMessages((prev) =>
            prev.map((message) =>
              message.id === targetId ? { ...message, retrieval } : message
            )
          );
        },
        controller.signal
      );
      // 回复完成后刷新会话列表，更新消息数与最近活跃时间。
      void refreshSessions();
    } catch {
      if (controller.signal.aborted) {
        // 用户主动停止：保留已生成内容；若还没吐出内容则给出停止提示。
        setMessages((prev) =>
          prev.map((m) =>
            m.id === targetId
              ? { ...m, content: acc || "（已停止回答）" }
              : m
          )
        );
        void refreshSessions();
      } else {
        // 网络/后端失败：标记 failed 并保存原始负载，供用户复用同一 client_message_id 重试。
        setMessages((prev) =>
          prev.map((m) =>
            m.id === targetId
              ? {
                  ...m,
                  content: "抱歉，暂时没能连上知镜，请稍后再试。",
                  failed: true,
                  retry: { clientMessageID, text },
                }
              : m
          )
        );
      }
    } finally {
      abortRef.current = null;
      setIsSending(false);
    }
  };

  const handleSendMessage = async (text: string) => {
    const userMessage: Message = {
      id: Date.now().toString(),
      type: "user",
      content: text,
    };
    setMessages((prev) => [...prev, userMessage]);

    // 血糖录入 → 确认卡（本地演示交互，暂不走后端）。
    if (text.includes("血糖") && /\d/.test(text)) {
      const value = text.match(/\d+(\.\d+)?/)?.[0] || "0";
      const confirmation: Message = {
        id: Date.now().toString() + "-confirm",
        type: "confirmation",
        content: {
          type: "blood-sugar",
          data: { label: "今日血糖", value, unit: "mmol/L" },
        },
      };
      setPendingConfirmation(confirmation);
      return;
    }

    // 其余自由对话走真实 chat 接口。先插入空占位气泡（显示三点动画），返回后原地替换。
    const typingId = Date.now().toString() + "-typing";
    setMessages((prev) => [
      ...prev,
      { id: typingId, type: "ai", content: "" },
    ]);

    const sessionID = activeSessionID ?? (await ensureSessionID());
    if (!activeSessionID) {
      setActiveSessionID(sessionID);
      rememberSessionID(sessionID);
    }
    // 全新发送生成新的 client_message_id。
    await streamAssistantReply(sessionID, text, crypto.randomUUID(), typingId);
  };

  // handleRetryMessage 重试失败的助手回复：复用原 client_message_id，让后端幂等去重
  // （已完成则回放、进行中则拒绝、失败/过期才真正重跑），不会重复扣费或重复落库。
  const handleRetryMessage = async (messageId: string) => {
    if (isSending) return;
    const target = messages.find((m) => m.id === messageId);
    if (!target?.retry) return;
    const { clientMessageID, text } = target.retry;
    const sessionID = activeSessionID ?? (await ensureSessionID());
    setMessages((prev) =>
      prev.map((m) =>
        m.id === messageId
          ? { ...m, content: "", failed: false, retry: undefined }
          : m
      )
    );
    await streamAssistantReply(sessionID, text, clientMessageID, messageId);
  };

  const handleAcceptMeal = () => {
    setMessages((prev) => [
      ...prev,
      {
        id: Date.now().toString(),
        type: "ai",
        content:
          "太棒了！记得慢慢咀嚼，让身体更好地吸收营养。餐后记得测一下血糖哦～",
      },
    ]);
  };

  const handleRegenerateMeal = () => {
    const newMeal: Message = {
      id: Date.now().toString(),
      type: "meal-suggestion",
      content: {
        meal: "🌅 午餐建议",
        title: "烤三文鱼配时蔬",
        emoji: "🐟",
        description: "富含 Omega-3 的三文鱼配上低碳水蔬菜，既美味又健康。",
        ingredients: ["三文鱼", "西兰花", "芦笋", "柠檬", "橄榄油"],
        benefits:
          "三文鱼中的 Omega-3 脂肪酸有助于改善胰岛素敏感性，搭配高纤维蔬菜能有效控制血糖上升速度。",
        gi: 28,
      },
    };
    setMessages((prev) => [...prev, newMeal]);
  };

  const handleToggleAction = (id: string) => {
    const updated = actions.map((a) =>
      a.id === id ? { ...a, completed: !a.completed } : a
    );
    setActions(updated);
    setMessages((prev) =>
      prev.map((msg) =>
        msg.type === "action-card" && msg.id === latestActionId
          ? { ...msg, content: updated }
          : msg
      )
    );
  };

  const handleConfirmData = () => {
    if (pendingConfirmation) {
      setMessages((prev) => [...prev, pendingConfirmation]);
      setPendingConfirmation(null);
      setTimeout(() => {
        setMessages((prev) => [
          ...prev,
          {
            id: Date.now().toString(),
            type: "ai",
            content:
              "收到！你的血糖数据已记录。目前数值在正常范围内，继续保持哦～",
          },
        ]);
      }, 500);
    }
  };

  const handleCancelConfirmation = () => {
    setPendingConfirmation(null);
  };

  // handleStop 用户主动中止正在进行的流式回复。
  const handleStop = () => {
    abortRef.current?.abort();
  };

  // handlePhoto 拍照/选图入口。图片识别后端管线尚未接入，先给出本地占位回复。
  const handlePhoto = (file: File) => {
    const base = Date.now().toString();
    setMessages((prev) => [
      ...prev,
      { id: base + "-photo-user", type: "user", content: "📷 [已上传照片]" },
      {
        id: base + "-photo-ai",
        type: "ai",
        content:
          "已收到照片～ 图片识别功能正在开发中，很快就能帮你分析饮食和营养了。",
      },
    ]);
    void file;
  };

  // 对话是否已开始：只剩欢迎语且无待确认卡时视为未开始，此时欢迎语居中显示。
  const conversationStarted =
    pendingConfirmation != null || messages.some((m) => m.id !== "welcome");

  return (
    <div className="size-full flex flex-col overflow-hidden bg-background relative">
      {profileOpen ? (
        <ProfileDashboard onBack={() => setProfileOpen(false)} />
      ) : activeView === "practice" ? (
        <>
          <AppHeader
            onOpenSessions={() => setSessionDrawerOpen(true)}
            onOpenProfile={() => setProfileOpen(true)}
          />

          <StatusTags tags={tags} />

          <div ref={scrollRef} className="flex-1 overflow-y-auto">
            {!conversationStarted ? (
              <div className="min-h-full flex flex-col items-center justify-center px-6 pb-48 text-center">
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
        <div className="flex flex-col pt-2 pb-48">
          <AnimatePresence>
            {messages.map((message) => {
              switch (message.type) {
                case "morning-greeting":
                  return (
                    <MorningGreetingCard
                      key={message.id}
                      onReply={handleMorningReply}
                    />
                  );
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
                      onRetry={
                        message.failed
                          ? () => handleRetryMessage(message.id)
                          : undefined
                      }
                    />
                  );
                case "meal-card":
                  return <MealCard key={message.id} />;
                case "meal-suggestion":
                  return (
                    <MealSuggestionCard
                      key={message.id}
                      {...message.content}
                      collapsed={message.id !== latestMealId}
                      onAccept={handleAcceptMeal}
                      onRegenerate={handleRegenerateMeal}
                    />
                  );
                case "action-card":
                  return (
                    <ActionCard
                      key={message.id}
                      actions={
                        message.id === latestActionId
                          ? actions
                          : message.content
                      }
                      collapsed={message.id !== latestActionId}
                      onToggle={handleToggleAction}
                    />
                  );
                case "warning":
                  return (
                    <WarningCard key={message.id} {...message.content} />
                  );
                case "confirmation":
                  return (
                    <ConfirmationCard
                      key={message.id}
                      {...message.content}
                      onConfirm={handleConfirmData}
                      onCancel={handleCancelConfirmation}
                    />
                  );
                default:
                  return null;
              }
            })}
          </AnimatePresence>

          <AnimatePresence>
            {pendingConfirmation && (
              <ConfirmationCard
                key={pendingConfirmation.id}
                {...pendingConfirmation.content}
                onConfirm={handleConfirmData}
                onCancel={handleCancelConfirmation}
              />
            )}
          </AnimatePresence>
        </div>
            )}
          </div>

          <InputDock
            onSendMessage={handleSendMessage}
            onVoiceInput={() => {}}
            onPhoto={handlePhoto}
            isResponding={isSending}
            onStop={handleStop}
            models={MODELS}
            selectedModelID={selectedModelID}
            onSelectModel={(id) => {
              setSelectedModelID(id);
              rememberModelID(id);
            }}
          />

          <SessionDrawer
            open={sessionDrawerOpen}
            sessions={sessions}
            activeSessionID={activeSessionID}
            loading={sessionsLoading}
            error={sessionsError}
            busy={isSending}
            onClose={() => setSessionDrawerOpen(false)}
            onSelect={handleSelectSession}
            onCreate={handleCreateSession}
            onRetry={refreshSessions}
            onRename={handleRenameSession}
            onDelete={handleDeleteSession}
          />
        </>
      ) : activeView === "plan" ? (
        <Dashboard mode="page" onOpenProfile={() => setProfileOpen(true)} />
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
