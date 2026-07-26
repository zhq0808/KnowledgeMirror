-- 回滚 000009：只删除检索审计表。
-- 授权、资料与片段数据从未被本迁移改动，因此回滚不会影响可召回集合的判定。
DROP TABLE IF EXISTS retrieval_hits;
DROP TABLE IF EXISTS retrieval_requests;
