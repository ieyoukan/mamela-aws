# mamela-aws

[Mamela](https://github.com/ieyoukan/mamela) の AWS のプロバイダーです。専用の VPC の中に、EC2 のインスタンス、ECS のクラスター、Fargate（Spot）や EC2 のサービスを作ります。

- **隔離して動く:** プロバイダーは WASM で動きます。AWS には Mamela の host を通してしかつなげません。
- **host が持つもの:** AWS の資格情報（profile。MFA や assume-role の一時的なものでもよい）と、運用者が選んだ API のカタログです。
- **host がすること:** 各要求に SigV4 で署名し、カタログと、その変更の許可（permit）に合う要求だけを送ります。プロバイダーは鍵を見ません。
- **IAM の制限:** IAM のカタログは、パス `/mamela/` のロール、ECS のタスクか EC2 が引き受ける信頼ポリシー、AWS 管理の 2 つの ECS のポリシーしか許しません。プロバイダーが何を送っても、これ以外の IAM の変更は host が拒みます。

設計は Mamela の ADR 0006（guard、明示操作）と ADR 0007（依存、AWS の JSON の API、一時的な資格情報、複数の API）にあります。

## API のカタログ（Mamela の ADR 0008）

カタログは、各呼び出しが何をするかを書いたデータです。効果、許可の ID への束縛（bind）、許す値（values）、大きさの測り方（measures）を持ちます。この Pome は、5 つの API のカタログを `pome/catalogs/*.json` に同梱しています。

- **使われ方:** Mamela は、運用者が `mamela.providers.toml` で名指したときだけ使います（`catalog = "pome:ec2"`）。
- **プログラムとの関係:** プロバイダーのプログラムはカタログを見ません。Mamela が、プログラムのすべての要求を、このカタログで分類します。
- **IAM の制限:** IAM のカタログは、パス `/mamela/`、ECS のタスクか EC2 の信頼ポリシー、AWS 管理の 2 つの ECS のポリシーしか許しません。プログラムが何を送っても、これ以外の IAM の変更は Mamela が拒みます。引き継ぎのために足した `UpdateRole` は、ロールの名前に束縛し、送れるのは説明（Mamela の目印）だけです。
- **使う前に読む:** `mamela provider catalog pome` で中身と sha256 を見られます。

### AWS の公式モデルから作る

カタログは手で書かず、AWS の公式の API モデルから生成します（[cmd/aws-catalogs](cmd/aws-catalogs)）。

- **モデル:** [aws/api-models-aws](https://github.com/aws/api-models-aws) の Smithy JSON です。EC2・ECS・CloudWatch Logs・IAM・SSM の 5 つを使います。
  - botocore の `service-2.json` ではなく Smithy にした理由: AWS の SDK すべての元になる正本で、プロトコル（`ec2Query`・`awsQuery`・`awsJson1_1`）、EC2 のクエリでの名前（`ec2QueryName`）、リストの書き方（`xmlFlattened`）、`@readonly`・`@deprecated` を、トレイトとしてそのまま持つからです。
- **固定:** [catalogs/models.lock.json](catalogs/models.lock.json) に、モデルごとのリポジトリ・コミット・パス・sha256 を書きます。モデル自体はコミットしません。
- **取得:** `go run ./cmd/aws-catalogs fetch` だけがネットワークを使います。固定したコミットのファイルを GitHub から落とし、sha256 が一致するものだけを `.cache/aws-models/`（git は無視）に置きます。
- **生成器が確かめること:** どれか 1 つでも外れれば、生成は失敗します（fail closed）。
  - カタログの各操作と各パラメータが、モデルにある。名前は、プロトコルが書く形で確かめる（EC2 の `Filter.1.Value.1`、IAM の `Tags.member.1.Key`、JSON の API の一段目のメンバー）。
  - プロバイダーが宣言するすべての呼び出しが、モデルの操作とパラメータで、版と `X-Amz-Target` もモデルと同じ。
  - 非推奨（`@deprecated`）の操作やパラメータを使っていない。
- **モデルから取るもの:** パラメータの置き場所（フォームか、`X-Amz-Target` ヘッダーと JSON の本文か）、版（`Version`）、`X-Amz-Target`（`<サービス>.<操作>`）です。
- **効果の分類:** 操作の名前の動詞で決めます（[catalogs/effects.go](catalogs/effects.go)）。

  | 動詞 | 効果 |
  |---|---|
  | Describe・Get・List | read |
  | Create・Run・Register | create |
  | Delete・Terminate・Deregister | lose-data |
  | Start・Stop・Reboot | restart |
  | Modify・Update・Put・Attach・Detach・Authorize・Revoke・Associate・Disassociate・Tag・Untag・Add・Remove | change |

  - **例外表:** 規則と違うものは、理由を付けてレビューした表に書きます。`CreateTags`・`CreateRoute` は change、`DeregisterTaskDefinition` は change です（ECS は INACTIVE で残し、使っているサービスは動き続ける）。
  - **分類できない動詞:** 生成は失敗します。推測はしません。
  - **`@readonly`:** モデルが読み取り専用とする操作が read 以外に分類されれば、失敗します。
- **重ねるもの（overlay）:** モデルに書いていない安全の決めごとは、人がレビューする [catalogs/overlay.go](catalogs/overlay.go) に書きます。
  - 各操作に送ってよいパラメータ
  - `bind`
  - `values`（IAM の制限）
  - `measures`
  - `RunInstances` の `MinCount`・`MaxCount` の固定

  生成器は、overlay が名指すものがすべてモデルにあることを確かめます。
- **試験:**
  - 同梱する JSON が overlay と規則に合うこと（モデルなしで、いつも）
  - モデルがキャッシュにあれば、生成し直したものと同梱する JSON がバイト単位で同じこと（なければ skip。CI はモデルを取得して確かめる）
  - IAM の制限が、前より緩くないこと

### AWS の API が変わったときの更新手順

1. **固定を上げる:** `go run ./cmd/aws-catalogs relock <api-models-aws のコミット>`。各モデルを取得し、sha256 を `catalogs/models.lock.json` に書きます。
2. **取得する:** 別の環境では `go run ./cmd/aws-catalogs fetch` で、固定どおりのモデルを取得します。
3. **生成する:** `./build.sh`（または `go run ./cmd/aws-catalogs generate pome/catalogs`）。
   - 操作やパラメータが消えた、名前が変わった、非推奨になった、分類できない動詞が増えた、などのときは、ここで失敗します。overlay（またはプロバイダーの呼び出し）を直します。
4. **差分を読む:** `git diff pome/catalogs`。特に、どの呼び出しの効果（`effect`）が変わったか、`bind`・`values` が変わっていないかを読みます。
5. **試験する:** `go test ./...`。
6. **コミットする:** lock、カタログ、Pome（`pome/provider.wasm` など）を一緒にコミットします。運用者は、新しい版のカタログを読んでから使います（カタログが変われば、承認は効かなくなります）。

## リソース

すべてのリソースは、ある境界（`Aws.Boundary`）の中で宣言します。作るものすべてに、タグ `mamela:name`（宣言の名前）と `mamela:boundary` を付けます。AWS の名前は境界の名前で始まります（`locust-main`）。

| kind | 作るもの | plan の変更 |
|---|---|---|
| `AwsVpc` | VPC | `create` |
| `AwsInternetGateway` | インターネットゲートウェイ | `create`、`attach` |
| `AwsSubnet` | サブネット | `create` |
| `AwsRouteTable` | ルートテーブル（0.0.0.0/0 をゲートウェイへ、サブネットに関連付け） | `create`、`route` |
| `AwsSecurityGroup` | セキュリティグループと、TCP のインバウンドの規則 | `create`、`ingress`（規則は足すだけ） |
| `AwsLogGroup` | CloudWatch Logs のロググループ `/mamela/<境界>/<名前>` | `create`、`retention`（保存期間を延ばす。ログがまだないときは短くもする） |
| `AwsIamRole` | `/mamela/` のロール（EC2 用はインスタンスプロファイルも）。タグは付けず、説明に目印（`Mamela: mamela:boundary=… mamela:name=…`）を書く | `create`、`policy` |
| `AwsEcsCluster` | ECS のクラスター（Fargate と Fargate Spot。Container Insights は無効） | `create` |
| `AwsInstance` | ECS 最適化 Amazon Linux 2023 の EC2 インスタンス（IMDSv2 のみ）。次のどちらかをする。<br>・ロールのインスタンスプロファイルで、クラスターに参加する<br>・IAM ロールなしで、コンテナーを 1 つ Docker で動かす<br>起動スクリプトはプロバイダーが書く。宣言が渡せるのは、検査したイメージ名とポートだけ。<br>引き継いだインスタンスは、どちらもせず、イメージと起動スクリプトをそのまま保つ | `create`、`start`、`groups`（セキュリティグループ） |
| `AwsEcsTaskDefinition` | タスク定義（ファミリー `mamela-<境界>-<名前>`） | `create`、`revise`（新しいリビジョンを登録して、古いものを登録解除） |
| `AwsEcsService` | サービス | `create`、`update`（タスクの数、タスク定義、Fargate と Fargate Spot の切り替え、サブネット・セキュリティグループ・パブリック IP。新しいデプロイになる） |

- **宣言の書き方:** 種類ごとの型と関数は、マニフェストから生成した `Aws.Kinds`（[pome/src/kinds.emel](pome/src/kinds.emel)）にあります。手で書いた `Aws`（[pome/src/lib.emel](pome/src/lib.emel)）も使えます。
  - タスク定義のコンテナーの名前は `containerName`、環境変数の名前は `variable` です（生成器が、オブジェクトの `name` というフィールドを受け付けないため）。
- **依存:** 別のリソースの ID が要るリソース（VPC の中のサブネットなど）は、Pome が `after` を付けます。依存先ができるまで、そのリソースは待ちます（MAM-PLAN-0113）。依存の深さの回数だけ plan と apply を繰り返すと、すべてができます。
- **しないこと:** 次の差は、plan に示し（`replace` など）、Mamela が拒みます。理由と直し方も示します。
  - VPC・サブネットの CIDR やゾーン、クラスター、EC2 と Fargate の切り替え、インスタンスのサブネット・ロール・起動スクリプト。作り直すには、新しく宣言して、古いものを明示操作の `delete` で消します。
  - インスタンスのタイプ（`retype`）。インスタンスが止まるので、明示操作の `resize` で変えます。
  - ログが残っているロググループの保存期間を短くすること（`shorten`）。古いログが消えるので、明示操作の `shorten-retention` で変えます。
  - 規則やポリシーを外すこと。
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
| `adopt` | すべての kind | 取り込んだもの（`mamela import terraform`）に Mamela の印を付けて、Mamela のものにする。印のほかは何も変えない |
| `run-task` | `AwsEcsService` | そのサービスと同じ実行のしかた（起動タイプ、サブネット、セキュリティグループ）で、タスクを 1 つ動かす。負荷試験のような一回きりのタスクを、タスク数 0 のサービスから動かす |
| `resize` | `AwsInstance` | 宣言したタイプに変える。止めて（停止を待つ）、タイプを変えて、起動する。止まっているものは、タイプだけ変える（起動は次の plan）。パブリック IP は変わる |
| `reboot` | `AwsInstance` | 再起動する。痕跡が残らないので、応答が失われると、lookup は「分からない」と答える |
| `shorten-retention` | `AwsLogGroup` | 宣言した、より短い保存期間にする。それより古いログは消える |

`run-task` のタスクには、トークンのタグ（`mamela:run`）を付けます。応答が失われても、lookup はそのタスクがあるかで判定し、二重に動かしません。

### guard（適用の前後の安全確認）

`mamela.providers.toml` の `[provider.aws.guard]` に、境界と予算を書きます。guard は読み取りだけです。

| コード | 前 / 後 | 確かめること |
|---|---|---|
| `AWS-0001` | 前・後 | 設定（`boundary`、`instance_types`、`max_instances`、`max_tasks`）があること |
| `AWS-0100` | 前 | 各要求が名指すもの（VPC、サブネット、グループ、インスタンス、ロール、クラスター、サービス、タスク定義、ロググループ）が境界のものであること。作るものが境界のタグと名前を持つこと。インスタンスの型（起動、`resize`、引き継ぎ）が `instance_types` にあること。引き継ぎ（下）の印が、この境界と宣言の名前であること |
| `AWS-0103` | 前・後 | 境界の EC2 インスタンス（引き継ぐものも数える）が `max_instances` 以下であること |
| `AWS-0104` | 前・後 | 境界の ECS のタスク（サービスのタスク数、引き継ぐサービスのタスク数、`run-task` の動いているタスク）が `max_tasks` 以下であること |
| `AWS-0201` | 後 | 境界の外の EC2 インスタンスの状態が変わっていないこと（変われば warn。引き継いだものは除く） |
| `AWS-0202` | 後 | 境界の外の ECS クラスターが消えていないこと（引き継いだものは除く） |

引き継ぎ（`adopt`）について、guard は次のことも確かめます。

- 別の境界の Mamela のものには、印を付けない。
- サービスは、そのクラスターを先に引き継ぐ。
- タスク定義は、ファミリーが `mamela-<境界>-` で始まる。
- ロググループは、`/mamela/<境界>/` の下にある。
- ロールは、パスが `/mamela/` である（guard が読んで確かめる）。

## Terraform からの乗り換え（Mamela の ADR 0009）

Terraform（OpenTofu）で作ったものを、作り直さずに Mamela へ移せます。Mamela は Terraform を実行せず、取り込むだけでは AWS に何も送りません。引き継ぎ（`adopt`）が送るのは、Mamela の印を付ける要求 1 つだけです。

| Terraform の型 | 宣言 | 引き継ぎで付ける印 | 取り込まないもの（Terraform が管理し続ける） |
|---|---|---|---|
| `aws_vpc` | `AwsVpc` | タグ（`CreateTags`） | |
| `aws_internet_gateway` | `AwsInternetGateway` | タグ | VPC を取り込んでいない |
| `aws_subnet` | `AwsSubnet` | タグ | VPC を取り込んでいない |
| `aws_route_table`（`aws_route_table_association`・`aws_route` を含む） | `AwsRouteTable` | タグ | 0.0.0.0/0 をインターネットゲートウェイへ送る経路しかない、1 つのサブネットのためのもの以外 |
| `aws_security_group`（`aws_security_group_rule`・`aws_vpc_security_group_ingress_rule` を含む） | `AwsSecurityGroup` | タグ | ― |
| `aws_iam_role`（`aws_iam_role_policy_attachment`・`aws_iam_instance_profile` を含む） | `AwsIamRole` | 説明に目印（`UpdateRole`） | パスが `/mamela/` でない。信頼ポリシーが ECS のタスクか EC2 だけでない。ポリシーが 2 つの ECS のポリシーの 1 つだけでない。インラインポリシーがある。インスタンスプロファイルがロールと同じ名前で `/mamela/` にない |
| `aws_cloudwatch_log_group` | `AwsLogGroup` | タグ（`TagResource`） | 名前が `/mamela/<境界>/` の下にない。保存期間が無期限 |
| `aws_ecs_cluster`（`aws_ecs_cluster_capacity_providers` を含む） | `AwsEcsCluster` | タグ（`TagResource`） | ― |
| `aws_ecs_task_definition` | `AwsEcsTaskDefinition` | タグと、内容の digest | ファミリーが `mamela-<境界>-` で始まらない。宣言できない設定（タスクロール、ボリューム、ヘルスチェック、awslogs 以外のログなど）がある。実行ロールやロググループを取り込めない |
| `aws_ecs_service` | `AwsEcsService` | タグ（`TagResource`） | クラスターやタスク定義を取り込めない。ロードバランサーやサービスディスカバリーがある。サブネットかセキュリティグループが 1 つでない。DAEMON。ECS 以外のデプロイ |
| `aws_instance` | `AwsInstance` | タグ（`CreateTags`） | セキュリティグループが 1 つでない。サブネット・グループ・インスタンスプロファイルのロールを取り込めない |

- **境界:** 取り込んだものは、guard の境界（`[provider.aws.guard] boundary`）に入ります。
- **名前の決まり:** IAM のロールのパス、ロググループの名前、タスク定義のファミリーは、AWS では後から変えられません。Mamela はこれらで自分のものを探し、guard もこれらで境界を確かめます。そのため、決まりに合わないものは取り込まず、理由と次のどちらかのやり方を表示します。
  - Terraform の側で名前を決まりに合わせてから（新しく作り直される）、取り込み直す。
  - Mamela で新しく宣言し、使う側をそちらに向け、古いものは Terraform に残す。
- **取り込まないものを参照するもの:** これも取り込みません（そのタスク定義を使うサービスなど）。理由に、参照先の理由も書きます。
- **引き継ぐインスタンス:** イメージと起動スクリプトは、そのまま保ちます（Mamela は比べません）。Mamela が保つのは、タイプ、サブネット、セキュリティグループ、インスタンスプロファイル、起動していることです。
- **タスク定義:** 引き継ぐリビジョンに、内容の digest（`mamela:digest`）を付けます。
  - 宣言が同じ内容なら、すぐ一致します。
  - 違えば、次の plan で新しいリビジョンを登録します（ファミリーは保つ）。
  - digest は、AWS が答える内容を、宣言が書ける部分だけに正規化して計算します（宣言の側と同じ計算）。
- **セキュリティグループのルール:** Mamela が書けるもの（TCP の 1 つのポートへ、CIDR か取り込んだグループから）だけを宣言します。ほかのルール（egress、ポートの範囲、自分から）はそのまま残ります。Mamela はルールを消しません。
- **そのまま残るもの:** ロググループの KMS キー、クラスターの設定とキャパシティプロバイダー、サービスの配置の設定は、Mamela は変えません。
- **要る権限:** 運用者のロールに、次のものが要ります（`mamela check` が示します）。
  - 観測: `logs:ListTagsForResource`、`iam:ListRoles`（ほかは今までどおり）
  - 引き継ぎ: `ec2:CreateTags`、`ecs:TagResource`、`logs:TagResource`、`iam:UpdateRole`（`/mamela/` のロールだけでよい）
  - 明示操作: `ec2:StopInstances`・`ec2:ModifyInstanceAttribute`・`ec2:StartInstances`（`resize`）、`ec2:RebootInstances`（`reboot`）、`logs:PutRetentionPolicy`（`shorten-retention`）

手順:

```sh
cd terraform-dir && terraform show -json > /tmp/state.json
mamela import terraform --state /tmp/state.json --providers mamela.providers.toml <pome-dir>
```

1. `<pome-dir>/src/imported.emel` に宣言が書かれます。この Pome の生成モジュール `Aws.Kinds`（`pome/src/kinds.emel`）の型と関数を使います。
2. 宣言で `import Imported` し、グラフに `|> Imported.imported` を足します。
3. `mamela plan` では、それぞれが「引き継ぐ対象」と表示されます（MAM-PLAN-0114）。何も変わりません。
4. 表示された `mamela act --confirm <name> adopt "<ID>" <pome-dir>` で、1 つずつ引き継ぎます。
   - **順番:** 参照されるものから引き継ぎます。VPC → ゲートウェイ・サブネット・セキュリティグループ → ルートテーブル → ロール・ロググループ・クラスター → タスク定義 → サービス・インスタンス。
   - サービスを、そのクラスターより先に引き継ごうとすると、guard が止めます。表示される順（アドレス順）のままにはしないでください。
5. 引き継いだ後の plan は、宣言との差だけを示します。取り込んだままの宣言なら、差はありません。
6. 表示された `terraform state rm ...` で、Terraform に忘れさせます。

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
go run ./cmd/aws-catalogs fetch   # AWS のモデルを取得する（固定したもの、sha256 を確かめる。最初の 1 回）
./build.sh                        # カタログ、pome/provider.wasm、pome/mamela-provider.toml、pome/src/kinds.emel を作る（Go 1.27.3）
go test ./...                     # 適合試験（Mamela の providertest）、guard、取り込み、カタログ、ビルドの再現性
```

- **作り:** Mamela の provider kit（`provider/kit`）の上で動きます。種類ごとの知識（一覧、差分、効いているか、明示操作、取り込み、引き継ぎ）は `aws/` の各種類にあり、`aws/kit.go` が kit につなぎます。
- **生成物:** `pome/src/kinds.emel` は、`go tool mamela provider pome --out src/kinds.emel pome` がマニフェストから生成します（手で直さない）。`pome/src/lib.emel` は手で書いています。カタログ（`pome/catalogs/*.json`）は `cmd/aws-catalogs` が生成します。

- **適合試験の要件:** Mamela が固定した wasmtime が要ります（`MAMELA_TOOLCHAIN_DIR`、CI を参照）。
- **偽の AWS:** 適合試験は、偽の AWS（[fakeaws](fakeaws)）の上で次を確かめます。
  - 依存の順に収束すること
  - 宣言を消したものを残すこと
  - どの要求で応答が失われても、送れなくても、やり直して収束すること
  - `run-task` と、消す順の明示操作
  - ECS on EC2（クラスターに参加するインスタンス）
  - IAM のタグを付けないこと（偽の AWS は、タグ付きの作成を AccessDenied で拒む）
  - 他人のインスタンス、クラスター、VPC に触れないこと
  - Terraform で作ったネットワークと、ECS・IAM・Logs・EC2 のスタックを取り込み、引き継ぐと差分がなくなること（応答の喪失・未送信も含む）。取り込んだ後に宣言を変えれば、plan どおりに変わること
  - 宣言と違う世界での明示操作（`resize`、`reboot`、`shorten-retention`）。各要求の未送信・応答の喪失から、やり直して終わること（Mamela の host の代わりの小さな host を、試験の中で動かす: `aws/harness_test.go`）
  - guard が、境界の外への変更と、別の境界のものの引き継ぎを止めること
  - `mamela import terraform` の通し（state の JSON から Emela を書き、宣言として評価できること）
- **Pome の試験:** Emela の Pome は `pome/` にあり、`emela test` で確かめます。
- **公開前の注意:** Mamela Pome 0.3.0 と、この Pome の公開までは、`[replace]` で隣の mamela のチェックアウトを使います。

公開リポジトリです。アカウント ID、ARN、profile の名前、アドレスは書きません。それらは git が無視する `mamela.providers.toml` にだけ書きます。
