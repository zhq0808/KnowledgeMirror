package service

import (
	"path/filepath"
	"testing"
)

func TestTermGlossaryDetectsKnownMistranscription(t *testing.T) {
	glossary := NewTermGlossary([]string{
		"# 注释行应被忽略",
		"",
		"幂等|密等,读百",
		"状态机|状态基",
	})

	terms := glossary.AmbiguousTerms("这里靠密等保证重复请求只生效一次", 5)
	if len(terms) != 1 || terms[0].Term != "幂等" || terms[0].Heard != "密等" {
		t.Fatalf("AmbiguousTerms() = %+v, want [{幂等 密等}]", terms)
	}
}

// 讲对了就不该被打扰：术语原样出现时必须一条提示都不给，
// 否则每句话都弹确认，用户会直接把语音关掉。
func TestTermGlossaryIgnoresCorrectTerm(t *testing.T) {
	glossary := NewTermGlossary([]string{"幂等|密等", "RocketMQ|火箭MQ"})
	if terms := glossary.AmbiguousTerms("幂等和 RocketMQ 我都讲清楚了", 5); len(terms) != 0 {
		t.Fatalf("AmbiguousTerms() = %+v, want 空", terms)
	}
}

// 归一化只去空格和标点：Rocket MQ / rocket-mq 都是同一个词，不该被当成听错。
func TestTermGlossaryNormalizesSpacingAndCase(t *testing.T) {
	glossary := NewTermGlossary([]string{"RocketMQ"})
	if terms := glossary.AmbiguousTerms("我们后来换成了 rocket-mq", 5); len(terms) != 0 {
		t.Fatalf("AmbiguousTerms() = %+v, want 空", terms)
	}
}

// 拉丁术语允许一点点转写偏差；中文靠词表，不靠编辑距离（同音字编辑距离无效）。
func TestTermGlossaryFuzzyMatchesLatinTerms(t *testing.T) {
	glossary := NewTermGlossary([]string{"Kafka"})
	terms := glossary.AmbiguousTerms("消息走的是 kafak 那条链路", 5)
	if len(terms) != 1 || terms[0].Term != "Kafka" || terms[0].Heard != "kafak" {
		t.Fatalf("AmbiguousTerms() = %+v, want [{Kafka kafak}]", terms)
	}
}

func TestTermGlossaryDoesNotFuzzyMatchUnrelatedWords(t *testing.T) {
	glossary := NewTermGlossary([]string{"Kafka"})
	if terms := glossary.AmbiguousTerms("我们用的是 postgres 和 redis", 5); len(terms) != 0 {
		t.Fatalf("AmbiguousTerms() = %+v, want 空", terms)
	}
}

func TestTermGlossaryRespectsMaxAndNilReceiver(t *testing.T) {
	glossary := NewTermGlossary([]string{"幂等|密等", "状态机|状态基", "熔断|容断"})
	if terms := glossary.AmbiguousTerms("密等 状态基 容断", 2); len(terms) != 2 {
		t.Fatalf("AmbiguousTerms() 返回 %d 条, want 2", len(terms))
	}

	var missing *TermGlossary
	if terms := missing.AmbiguousTerms("密等", 5); terms != nil {
		t.Fatalf("词表未加载时应安静地不提示, got %+v", terms)
	}
	if size := missing.Size(); size != 0 {
		t.Fatalf("Size() = %d, want 0", size)
	}
}

// 仓库自带的词表必须能被加载：它是配置文件，写坏了应该在测试里就发现。
func TestLoadTermGlossaryFromRepoFile(t *testing.T) {
	glossary, err := LoadTermGlossary(filepath.Join("..", "..", "prompts", "voice_glossary_v1.txt"))
	if err != nil {
		t.Fatalf("LoadTermGlossary() error = %v", err)
	}
	if glossary.Size() < 10 {
		t.Fatalf("Size() = %d, 词表内容疑似没被解析", glossary.Size())
	}
	terms := glossary.AmbiguousTerms("这段用密等挡住重复请求", 5)
	if len(terms) != 1 || terms[0].Term != "幂等" {
		t.Fatalf("AmbiguousTerms() = %+v, want 命中幂等", terms)
	}
}

func TestLoadTermGlossaryMissingFile(t *testing.T) {
	if _, err := LoadTermGlossary(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("LoadTermGlossary() 文件不存在时应报错，由调用方决定降级还是失败")
	}
}
