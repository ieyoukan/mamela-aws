# mamela-aws

[Mamela](https://github.com/ieyoukan/mamela) の AWS のプロバイダーです。専用の VPC の中に、EC2 のインスタンス、ECS のクラスター、Fargate（Spot）や EC2 のサービスを作ります。

- **隔離して動く:** プロバイダーは WASM で動きます。AWS には Mamela の host を通してしかつなげません。
- **host が持つもの:** AWS の資格情報（profile。MFA や assume-role の一時的なものでもよい）と、Mamela の AWS のカタログ（`builtin:ec2`・`ecs`・`logs`・`iam`・`ssm`）。
- **host がすること:** 各要求に SigV4 で署名し、カタログと、その変更の許可（permit）に合う要求だけを送ります。プロバイダーは鍵を見ません。
- **IAM の制限:** IAM のカタログは、パス `/mamela/` のロール、ECS のタスクか EC2 が引き受ける信頼ポリシー、AWS 管理の 2 つの ECS のポリシーしか許しません。プロバイダーが何を送っても、これ以外の IAM の変更は host が拒みます。

設計は Mamela の ADR 0006（guard、明示操作）と ADR 0007（依存、AWS の JSON の API、一時的な資格情報、複数の API）にあります。

## リソース

すべてのリソースは、ある境界（`Aws.Boundary`）の中で宣言します。作るものすべてに、タグ `mamela:name`（宣言の名前）と `mamela:boundary` を付けます。AWS の名前は境界の名前で始まります（`locust-main`）。

| kind | 作るもの | plan の変更 |
|---|---|---|
| `AwsVpc` | VPC | `create` |
| `AwsInternetGateway` | インターネットゲートウェイ | `create`、`attach` |
| `AwsSubnet` | サブネット | `create` |
| `AwsRouteTable` | ルートテーブル（0.0.0.0/0 をゲートウェイへ、サブネットに関連付け） | `create`、`route` |
| `AwsSecurityGroup` | セキュリティグループと、TCP のインバウンドの規則 | `create`、`ingress`（規則は足すだけ） |
| `AwsLogGroup` | CloudWatch Logs のロググループ `/mamela/<境界>/<名前>` | `create`、`retention` |
| `AwsIamRole` | `/mamela/` のロール（EC2 用はインスタンスプロファイルも）。タグは付けず、説明に目印（`Mamela: mamela:boundary=… mamela:name=…`）を書く | `create`、`policy` |
| `AwsEcsCluster` | ECS のクラスター（Fargate と Fargate Spot。Container Insights は無効） | `create` |
| `AwsInstance` | ECS 最適化 Amazon Linux 2023 の EC2 インスタンス（IMDSv2 のみ）。次のどちらかをする。<br>・ロールのインスタンスプロファイルで、クラスターに参加する<br>・IAM ロールなしで、コンテナーを 1 つ Docker で動かす<br>起動スクリプトはプロバイダーが書く。宣言が渡せるのは、検査したイメージ名とポートだけ | `create`、`start` |
| `AwsEcsTaskDefinition` | タスク定義 | `create`、`revise`（新しいリビジョンを登録して、古いものを登録解除） |
| `AwsEcsService` | サービス | `create`、`update`（タスクの数、タスク定義） |

- **依存:** 別のリソースの ID が要るリソース（VPC の中のサブネットなど）は、Pome が `after` を付けます。依存先ができるまで、そのリソースは待ちます（MAM-PLAN-0113）。依存の深さの回数だけ plan と apply を繰り返すと、すべてができます。
- **しないこと:** CIDR、型、ネットワーク、インスタンスの役目の変更などは、差分としては示し（`replace`）、Mamela が拒みます。規則やポリシーを外すこともしません。
- **IAM ロールにタグを付けない理由:** タグには `iam:TagRole` が要ります。運用者のロールには、この権限がないことが珍しくありません（検証に使ったロールにもありませんでした）。持ち主は、パス `/mamela/`、説明の目印、Mamela の作成記録で確かめます。
- **ECS on EC2 に要る権限:** コンテナーインスタンス（クラスターに参加するインスタンス）には、運用者のロールに次の 2 つが要ります。
  - `/mamela/` のインスタンスプロファイルの権限（`iam:CreateInstanceProfile` など）
  - `/mamela/` のロールを EC2 に渡す `iam:PassRole`
  
  ない環境では、管理者が次のポリシーを運用者のロールに付けます（`<account>` は自分のアカウント）。範囲は `/mamela/` だけです。付けられないときは、コンテナーを動かすインスタンスと Fargate を使います。

  ```json
  {"Version": "2012-10-17", "Statement": [
    {"Effect": "Allow",
     "Action": ["iam:CreateInstanceProfile", "iam:DeleteInstanceProfile", "iam:AddRoleToInstanceProfile", "iam:RemoveRoleFromInstanceProfile"],
     "Resource": "arn:aws:iam::<account>:instance-profile/mamela/*"},
    {"Effect": "Allow", "Action": "iam:PassRole", "Resource": "arn:aws:iam::<account>:role/mamela/*",
     "Condition": {"StringEqualsIfExists": {"iam:PassedToService": "ec2.amazonaws.com"}}}
  ]}
  ```

  `AddRoleToInstanceProfile` も `iam:PassRole` を求めます。そのときは `iam:PassedToService` がないので、条件は `StringEqualsIfExists` にします。
- **宣言を消しても、何も消しません。** 消すのは、次の明示操作だけです。

### 明示操作

plan には入らず、人が名前を打ち直したときだけ行う操作です（`mamela act --confirm <名前> <操作> <リソース> <pome-dir>`）。対象は、Mamela が作成を記録したものに限ります。

| 操作 | 対象 | 内容 |
|---|---|---|
| `delete` | すべての kind | 消す（データを失う）。依存するものを先に消す |
| `run-task` | `AwsEcsService` | そのサービスと同じ実行のしかた（起動タイプ、サブネット、セキュリティグループ）で、タスクを 1 つ動かす。負荷試験のような一回きりのタスクを、タスク数 0 のサービスから動かす |

`run-task` のタスクには、トークンのタグ（`mamela:run`）を付けます。応答が失われても、lookup はそのタスクがあるかで判定し、二重に動かしません。

### guard（適用の前後の安全確認）

`mamela.providers.toml` の `[provider.aws.guard]` に、境界と予算を書きます。guard は読み取りだけです。

| コード | 前 / 後 | 確かめること |
|---|---|---|
| `AWS-0001` | 前・後 | 設定（`boundary`、`instance_types`、`max_instances`、`max_tasks`）があること |
| `AWS-0100` | 前 | 各要求が名指すもの（VPC、サブネット、グループ、インスタンス、ロール、クラスター、サービス、タスク定義、ロググループ）が境界のものであること。作るものが境界のタグと名前を持つこと。インスタンスの型が `instance_types` にあること |
| `AWS-0103` | 前・後 | 境界の EC2 インスタンスが `max_instances` 以下であること |
| `AWS-0104` | 前・後 | 境界の ECS のタスク（サービスのタスク数と、`run-task` の動いているタスク）が `max_tasks` 以下であること |
| `AWS-0201` | 後 | 境界の外の EC2 インスタンスの状態が変わっていないこと（変われば warn） |
| `AWS-0202` | 後 | 境界の外の ECS クラスターが消えていないこと |

## 例: locust の負荷試験（[examples/locust](examples/locust)）

Fargate Spot の locust が、EC2 のインスタンス上の nginx に負荷をかけます。どちらも専用の VPC の中にあり、境界の外のものには触れません。

churaverse_infra の terraform/locust との対応:

- **同じもの:** locust の構成。Fargate のタスク、専用のセキュリティグループ、7 日のロググループ、ターゲットの tcp/80 は locust のグループからだけ受ける。
- **違うもの:** ターゲット。元の ECS on EC2 のゲームサーバーではなく、専用の nginx にする。既存のサービスに負荷はかけない。

```text
VPC 10.42.0.0/16 ── internet gateway
 └ public subnet 10.42.1.0/24（route table: 0.0.0.0/0 → igw）
    ├ EC2 t4g.micro（ECS 最適化 AL2023、IAM ロールなし、SG target: tcp/80 は SG locust からだけ）
    │   └ docker: nginx（:80）
    └ ECS cluster locust-main ─ service locust（Fargate Spot、タスク数 0、SG locust）
        └ run-task: locust --headless -u 5 -r 1 -t 1m（LOCUST_HOST=http://<EC2 の private IP>）
CloudWatch Logs /mamela/locust/locust（7 日）、IAM /mamela/locust-task-execution
```

### 費用の目安（ap-northeast-1）

- **t4g.micro:** 1 時間あたり約 0.01 USD。ほかに、パブリック IPv4 と 30 GiB の gp3 がかかります。
- **Fargate Spot（0.25 vCPU、0.5 GiB）:** 1 分の試験 1 回で 0.001 USD 未満。
- **NAT ゲートウェイとロードバランサー:** 使いません。

試験が終われば、下の順に消します。

### 手順

1. **設定を作る:** `mamela.providers.example.toml` を `mamela.providers.toml`（git は無視）にコピーし、profile と region を書きます。guard の境界は、宣言の `Boundary` と同じにします。
2. **確かめる:** `mamela check examples/locust` と `mamela guard examples/locust` を実行します。
3. **作る:** `mamela plan examples/locust` と `mamela apply examples/locust` を、待つリソースがなくなるまで繰り返します。
4. **試験する:** `mamela act --confirm locust run-task "AwsEcsService locust" examples/locust` を実行します。locust の結果は `/mamela/locust/locust` のロググループに出ます。
5. **消す:** 宣言から消してから、次の順に `mamela act --confirm <名前> delete <リソース> examples/locust` を実行します。
   1. `AwsEcsService locust`
   2. `AwsInstance target`
   3. `AwsEcsCluster main`
   4. `AwsEcsTaskDefinition locust`
   5. `AwsLogGroup locust`
   6. `AwsIamRole task-execution`
   7. `AwsSecurityGroup target`、`AwsSecurityGroup locust`
   8. `AwsRouteTable public`
   9. `AwsSubnet public`
   10. `AwsInternetGateway igw`
   11. `AwsVpc net`

## 実機での検証（2026-11-11）

`examples/locust` を、MFA で assume-role した運用者のロールで、ap-northeast-1 に作り、試験し、消しました。

- **作成:** plan と apply を 6 周しました。依存の深さの分だけ周が要ります。待つリソースは MAM-PLAN-0113 と表示されます。
- **負荷試験:** `run-task` で 1 回、Fargate Spot で動かしました。結果は 196 リクエスト、失敗 0、中央値 2 ms で、locust の終了コードは 0 でした。
- **片付け:** 明示操作の `delete` で、15 のリソースをすべて消しました。
- **guard:** 毎回の前後で block はありませんでした。境界の外の EC2 インスタンスとクラスターは、変わっていません。
- **片付けの後の確認:** AWS CLI の読み取りで確かめました。リージョンの VPC と EC2 インスタンスの数は作る前と同じで、境界のものは何も残っていません。
- **費用:** t4g.micro が約 10 分、Fargate Spot の 0.25 vCPU が約 2 分。合わせて 0.01 USD 未満です。

### ECS on EC2（2 回目、同じ日）

churaverse の以前の dev 環境と同じ形を作りました。ECS 最適化 AL2023 の t4g.micro（ARM64）がクラスターに参加し、その上で EC2 のサービスが動きます。中身は、ゲームサーバーの代わりの nginx です。作り方は、その試験の間だけ、上の権限のポリシーを運用者のロールに付けて行いました。

- **作成:** plan と apply を 7 周しました。コンテナーインスタンスがクラスターに登録され、nginx のサービスは steady state になりました。
- **負荷試験:** `run-task` で locust を Fargate Spot で動かしました。196 リクエスト、失敗 0、終了コード 0 でした。
- **片付け:** 15 のリソースを `delete` で消しました。ECS が INACTIVE で残すタスク定義は、Mamela のカタログにない `DeleteTaskDefinitions` で、AWS CLI で消しました。1 回目のものも含みます。
- **権限を戻す:** 付けたポリシーを外して消しました。運用者のロールのポリシーは、前と同じ 19 で、インラインはありません。
- **元に戻ったこと:** CLI で確かめました。
  - IAM・EC2・ECS・Logs に、Mamela のものは何も残っていません。
  - VPC と EC2 インスタンスの数は、触る前と同じです。
  - terminate したインスタンスだけは、AWS の仕様で、しばらく一覧に見えます。

実機で見つけ、直したことがあります。

- **IAM のタグ:** 運用者のロールに `iam:TagRole` がなく、タグ付きの CreateRole が拒まれました。ロールにはタグを付けず、説明に目印を書くようにしました。
- **ECS on EC2:** 運用者のロールでは、インスタンスプロファイルを作れず、EC2 に PassRole できるロールも限られていました。そのため、`examples/locust` のターゲットは「IAM ロールのない EC2 の上のコンテナー」にしました。
- **Mamela 本体:** 次の 2 つを直しました。
  - メンバーのない JSON の呼び出しで、本文 `{}` を送る。
  - 観測の ID で、先頭の `/` を許す。CloudWatch Logs のロググループ名のため。
- **RunInstances の分類:** RunInstances の宣言に固定の `MinCount`・`MaxCount` がなく、承認が「カタログが知らない呼び出し」（MAM-POL-0208）になっていました。宣言に書き足し、すべての呼び出しが分類されることを試験するようにしました。

## 開発

```bash
./build.sh                  # pome/provider.wasm と pome/mamela-provider.toml を作る（Go 1.27.3）
go test ./...               # 適合試験（Mamela の providertest）、guard、ビルドの再現性
```

- **適合試験の要件:** Mamela が固定した wasmtime が要ります（`MAMELA_TOOLCHAIN_DIR`、CI を参照）。
- **偽の AWS:** 適合試験は、偽の AWS（[fakeaws](fakeaws)）の上で次を確かめます。
  - 依存の順に収束すること
  - 宣言を消したものを残すこと
  - どの要求で応答が失われても、送れなくても、やり直して収束すること
  - `run-task` と、消す順の明示操作
  - ECS on EC2（クラスターに参加するインスタンス）
  - IAM のタグを付けないこと（偽の AWS は、タグ付きの作成を AccessDenied で拒む）
  - 他人のインスタンス、クラスター、VPC に触れないこと
- **Pome の試験:** Emela の Pome は `pome/` にあり、`emela test` で確かめます。
- **公開前の注意:** Mamela Pome 0.3.0 と、この Pome の公開までは、`[replace]` で隣の mamela のチェックアウトを使います。

公開リポジトリです。アカウント ID、ARN、profile の名前、アドレスは書きません。それらは git が無視する `mamela.providers.toml` にだけ書きます。
