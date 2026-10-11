# 実行補助が検証するpacket

`scripts/agent_cycle.py`の管理セッションは、完了した仕事について以下のJSONをstate-dir内に保存し、最終結果の`packet`にそのパスを返します。`progress`には合格packetが必要です。最後の仕事を完了して`idle`になる場合もpacketを返します。完了した仕事がない場合だけ空文字にします。

これは [概念例](task-packet.example.json)とは異なる、実際の継続ゲートの形式です。実装をローカルcommitにし、その同一commitを独立レビュー・検証してください。state-dirのログはGitに追加しません。

```json
{
  "task_id": "ecs-recovery-case",
  "objective": "再現した復旧経路の不具合を修正する",
  "acceptance": ["同一操作でprovider writeが最大1回である"],
  "implementer": "implementation-agent",
  "candidate_commit": "<現在のHEADの完全なcommit ID>",
  "product_decision": {
    "basis": "accepted_issue",
    "reference": "https://github.com/yingtian0/Themisy/issues/8"
  },
  "review": {
    "reviewer": "independent-review-agent",
    "verdict": "pass",
    "commit": "<同じcommit ID>",
    "log": "evidence/review.txt"
  },
  "required_checks": ["postgres-recovery"],
  "checks": [
    {"name": "format", "command": "make fmt-check", "commit": "<同じcommit ID>", "exit_code": 0, "log": "evidence/format.log"},
    {"name": "unit", "command": "make test", "commit": "<同じcommit ID>", "exit_code": 0, "log": "evidence/unit.log"},
    {"name": "race", "command": "make test-race", "commit": "<同じcommit ID>", "exit_code": 0, "log": "evidence/race.log"},
    {"name": "lint", "command": "make lint", "commit": "<同じcommit ID>", "exit_code": 0, "log": "evidence/lint.log"},
    {"name": "postgres-recovery", "command": "<実際に実行した対象試験コマンド>", "commit": "<同じcommit ID>", "exit_code": 0, "log": "evidence/postgres.log"}
  ],
  "ci": {"status": "not_run", "commit": "<同じcommit ID>"}
}
```

例のplaceholderは実値に置換します。`product_decision.basis`は`accepted_issue`または`independent_review`です。後者は`reference`に加え、異なる`proposer`と`reviewer`を記録します。

`review.verdict`は`pass`、reviewerはimplementerと別の識別子であることが必要です。`required_checks`は変更固有の必須試験名です。それとは別に`format`、`unit`、`race`、`lint`は常に必要です。各checkのnameは重複不可、exit_codeは整数の0、commandは実行コマンド、commitはcandidateと同一にします。

`ci.status`は`passed`または`not_run`です。`passed`には同じcommitのrun URLを`url`に記録します。`not_run`なら記録されるverificationは`local_only`です。failed/pendingは合格packetにはできません。未実施のremote CIを成功と扱わず、必要な場合は残作業として引き継ぎます。

packetと各logはstate-dir内の存在する空でないファイルであることが必要です。親ディレクトリへの参照やsymlinkによる外への参照は拒否されます。HEADの変更、trackedファイルの未commit差分、state-dir外の未追跡ファイルがあると証拠は無効です。実行補助はpacketのSHA-256、candidate commit、検証区分を状態に保存します。

## ゲートが保証しないこと

この検証は**記録の整合性**を確認します。ログが本当にそのコマンドの出力か、reviewer IDが実在する別エージェントか、Issueが実際に承認済みか、CI URL先が成功したかを独立に証明するものではありません。管理エージェントは実際の担当者・ツール結果を保存し、虚偽の記録や別commitの成功を流用してはいけません。

packet hashはpacketファイルに対するもので、各logの内容を署名・固定するものではありません。ignoredファイル、外部依存、実行環境の完全性もこのゲートだけでは保証しません。3回の修正上限や受入条件の十分性は現状では管理者の運用責任です。これらの限界を、製品のproduction-readinessや自動リリースの保証に読み替えないでください。
