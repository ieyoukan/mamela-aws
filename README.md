# mamela-aws

[Mamela](https://github.com/ieyoukan/mamela) の AWS のプロバイダーです。専用の VPC の中に、ECS のクラスター、EC2 のコンテナーインスタンス、Fargate（Spot）のサービスを作ります。

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
| `AwsIamRole` | `/mamela/` のロール（EC2 用はインスタンスプロファイルも） | `create`、`policy` |
| `AwsEcsCluster` | ECS のクラスター（Fargate と Fargate Spot。Container Insights は無効） | `create` |
| `AwsEcsInstance` | ECS 最適化 Amazon Linux 2023 の EC2 インスタンス（クラスターに参加。IMDSv2 のみ） | `create`、`start` |
| `AwsEcsTaskDefinition` | タスク定義 | `create`、`revise`（新しいリビジョンを登録して、古いものを登録解除） |
| `AwsEcsService` | サービス | `create`、`update`（タスクの数、タスク定義） |

- **依存:** 別のリソースの ID が要るリソース（VPC の中のサブネットなど）は、Pome が `after` を付けます。依存先ができるまで、そのリソースは待ちます（MAM-PLAN-0113）。依存の深さの回数だけ plan と apply を繰り返すと、すべてができます。
- **しないこと:** CIDR、型、ネットワークの変更などは、差分としては示し（`replace`）、Mamela が拒みます。規則やポリシーを外すこともしません。
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

Fargate Spot の locust が、EC2 のコンテナーインスタンス上の nginx（ECS のサービス）に負荷をかけます。どちらも専用の VPC の中にあり、境界の外のものには触れません。

```text
VPC 10.42.0.0/16 ── internet gateway
 └ public subnet 10.42.1.0/24（route table: 0.0.0.0/0 → igw）
    ├ EC2 t4g.micro（ECS 最適化 AL2023、SG target: tcp/80 は SG locust からだけ）
    │   └ service target: nginx（EC2、bridge、hostPort 80）
    └ service locust（Fargate Spot、タスク数 0、SG locust）
        └ run-task: locust --headless -u 5 -r 1 -t 1m（LOCUST_HOST=http://<EC2 の private IP>）
CloudWatch Logs /mamela/locust/locust（7 日）、IAM /mamela/locust-task-execution・locust-instance
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
   1. `AwsEcsService locust`、`AwsEcsService target`
   2. `AwsEcsInstance target`
   3. `AwsEcsCluster main`
   4. `AwsEcsTaskDefinition locust`、`AwsEcsTaskDefinition nginx`
   5. `AwsLogGroup locust`
   6. `AwsIamRole instance`、`AwsIamRole task-execution`
   7. `AwsSecurityGroup target`、`AwsSecurityGroup locust`
   8. `AwsRouteTable public`
   9. `AwsSubnet public`
   10. `AwsInternetGateway igw`
   11. `AwsVpc net`

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
  - 他人のインスタンス、クラスター、VPC に触れないこと
- **Pome の試験:** Emela の Pome は `pome/` にあり、`emela test` で確かめます。
- **公開前の注意:** Mamela Pome 0.3.0 と、この Pome の公開までは、`[replace]` で隣の mamela のチェックアウトを使います。

公開リポジトリです。アカウント ID、ARN、profile の名前、アドレスは書きません。それらは git が無視する `mamela.providers.toml` にだけ書きます。
