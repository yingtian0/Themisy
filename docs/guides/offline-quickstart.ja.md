# 15分で試すローカル計画

このガイドは、ECSと既存のCI/CDを使うPlatform/SRE担当者向けです。ローカルファイルだけを使い、クラウド認証情報、Control Plane、PostgreSQL、Temporal、Runnerは必要ありません。GrantやApprovalを作らず、認証情報を取得せず、Adapterによる書き込みも行いません。

入力値とContextはすべて合成データです。Planの生成で確認できるのは構造検証と計画だけです。Identity、CI、実行時の健全性、Policy承認、本番準備状況は検証しません。出力を実行権限として使わないでください。

## 準備

Go 1.26.9以降をインストールし、このガイドを含むcheckoutを使います。リポジトリのルートでCLIをビルドします。

```bash
go build -o /tmp/themisyctl ./cmd/themisyctl
```

初回ビルドではGo moduleのダウンロードが発生するため、インターネット接続が必要な場合があります。ビルド後のコマンドはネットワークを使いません。secretファイルや環境変数は不要です。`/tmp/themisyctl`が既に使われている場合は、自分で選んだ別の出力先にしてください。

## Planを生成する

```bash
/tmp/themisyctl offline-plan -at 2026-10-18T00:00:00Z examples/offline/release.json
```

終了コードは **0** です。標準エラーに`OFFLINE ONLY`、標準出力に`mode: "offline"`、注意書き、`plan`を含むJSONが出ます。Planは`identity-api`を`sha-demo`として`ecs-rolling-standard`を使う単一staging stepです。`plan.hash`と`plan.plan_hash`は同じ値です。Contract/Profile hashと`context_snapshot_ref`はサービスと同じplannerから算出されます。この練習には検証済みのPolicy/Evidence snapshotがないため、そのhashは空です。

`-at`で時刻を固定すると結果を再現できます。省略時は現在のUTC時刻を使います。この例のPlanは指定時刻から15分後に期限切れになります。offline計画は、期限切れPlanを実行可能にはしません。同じ入力・時刻・planner contextを使えば、完全なPlanとhashが一致します。

## 拒否例を見る

```bash
/tmp/themisyctl offline-plan examples/offline/rejected.json
```

終了コードは **1** で、標準出力にPlanは出ず、標準エラーに次の理由が出ます。

```text
themisyctl: unknown service "missing-api"
```

`examples/offline/rejected.json`の`changes[0].service`にはService Contractがありません。`identity-api`へ変えると、この拒否理由は解消します。Contract graphのエラーも既存の理由コード（例：`UNKNOWN_DEPENDENCY`）を返し、該当dependency fieldを示します。このケースはローカルテストで確認されています。

自分のファイルを試す場合は、Intentファイルより前にdirectoryを指定します。

```bash
/tmp/themisyctl offline-plan -contracts examples/contracts -profiles examples/profiles examples/offline/release.json
```

コマンドはすぐ終了します。停止するserviceやリセットする状態はありません。不要になった場合は、自分が作成したCLI binaryだけを削除してください。DB、container、cloudの後片付けはありません。

## 独立した利用観察を記録する

目標は、支援なしで通常例と拒否例を15分以内に再現できることです。build/download時間も含めます。開発者のテストは、外部利用者がこの目標を達成できる証明にはなりません。fresh checkoutで試し、次を記録してください。

| 項目 | 観察結果 |
|---|---|
| Commit、OS、Go version | 未計測 |
| 参加者・事前知識 | 未計測 |
| 開始・通常Plan・拒否例の時刻 | 未計測 |
| 支援内容・分かりにくかった箇所 | 未計測 |
| 通常例のexit 0・拒否例のexit 1 | 未計測 |
| 2回目の利用・独自Contract・準備負担 | 未計測 |

共有する観察に認証情報や個人情報を含めないでください。外部観察と継続利用の評価はIssue [#47](https://github.com/yingtian0/Themisy/issues/47)で追跡します。
