# ECS MVPの企画評価

確認日: 2026-10-11。固定ゴールは [implementation strategy](../implementation-strategy.md)に従います。

## ラベル

- **確認済み**: 指定した情報源または対象commitの検証で裏付けられた範囲。
- **仮説**: 顧客価値・比較優位など、今後検証する主張。
- **未確認**: 試験・調査が不足している項目。失敗とも成功とも扱わない。

コードが存在することと、release gateを通過したことは別々に記録します。実装済み機能を、古い引き継ぎだけを理由に未実装扱いしません。

## 代替手段との比較

| 評価 | ラベル | 根拠・次の検証 |
|---|---|---|
| GitHub ActionsはOIDCで長期AWS鍵の保存を避けられる | 確認済み（外部機能） | [GitHub公式](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-in-aws?apiVersion=2022-11-28)。鍵を保存しないこと単独では差別化にならない |
| ECSはdeployment circuit breakerと自動rollbackを提供する | 確認済み（外部機能） | [AWS公式](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/deployment-circuit-breaker.html)。障害検出・rollback単独では差別化にならない |
| CI/Agentにwrite資格情報を渡さず、承認・実操作・再送安全性・検証証拠を一貫して結び付ける価値 | 仮説 | 同じ脅威条件で既存CI/CD＋IAM構成と比較し、専用stagingで許可経路と迂回拒否を実証する |
| 導入・保守コストが既存構成より低い | 未確認 | 導入時間、必要な設定・運用作業、障害復旧時間を実測し顧客に確認する |

## 次の仕事の選び方

現行mainにはSQL状態更新修正とrecovery/manifestの基盤があります。新しい作業は現行コードとIssueの受入条件を照合して選びます。まず共有PostgreSQL、複数Runner、各crash pointのwrite回数、独立context照合の失敗経路の証拠不足を調べます。既に合格した条件は再実装せず、未達の境界を一つのpacketにします。

専用AWS stagingのdeploy/reconcile/rollback、実配置・health、署名result、Agent/CIのIAM迂回拒否は、それぞれ実施した証拠がある場合だけ合格にします。fakeテストからproduction-readyとは推論しません。

企画レビューには、対象ユーザー、既存の解決策、導入で増える負担、選ばないほうがよい条件、検証方法を必ず含めます。追加UI・別provider・別workflow engineより、現行ECS MVPの未達gateを優先します。
