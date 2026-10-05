#!/usr/bin/env python3
"""Write the Phase 1c matcher cases with the Python engine's answers.

    PYTHONPATH=services/elitea-deepwiki/src python parity/python_phase1c_cases.py \
        tests/fixtures/phase1c/api_surface_cases.json

Each case is one node (language, symbol name/type, rel_path, source text)
plus the orchestrator's extras (Pylon plugin name, file-wide gRPC stub
bindings). The output pairs it with what ``extract_api_surfaces`` returns —
``null`` when it raises — and adds ``to_snake`` / ``strip_common_api_prefix``
/ ``derive_pylon_endpoints`` samples. The Rust unit test
``graph::api_surface::tests`` replays every case against the port.
"""

from __future__ import annotations

import json
import sys

from elitea_deepwiki.engine.code_graph import api_surface_extractor as ase

CASES = [
    # ── REST ──
    ("python", "list_items", "function", "app/items.py",
     '@router.get("/items/")\n@router.head("/items")\ndef list_items(): ...'),
    ("python", "h", "function", "app/main.py",
     'router = APIRouter(prefix="/api/v1/users")\n@router.post("/me")\ndef h(): ...'),
    ("python", "idx", "function", "app/flask.py",
     '@bp.route("/a", methods=["GET", "post", \'PUT\', "BREW"])\ndef idx(): ...\n@app.route("/b")\ndef b(): ...'),
    ("python", "nope", "function", "app/x.py", "@app.get(x)\ndef nope(): ..."),
    ("typescript", "Ctl", "class", "src/ctl.ts",
     "@Controller('x')\nclass Ctl {\n  @Get()\n  a() {}\n  @Post(':id')\n  b() {}\n}"),
    ("javascript", "routes", "function", "src/app.js",
     "app.get('/api/users/:id', h); router.POST(\"/rest/v2beta3/things\", h); app.use(x, y)"),
    ("typescript", "client", "function", "src/client.ts",
     "axios.get('/api/v1/items'); client.post(`/graphql`); api.delete(\"/x/\");"
     " fetch('/api/login', { method: 'post', body }); fetch(\"/health\")"),
    ("typescript", "req", "function", "src/gen.ts",
     "return __request(OpenAPI, {\n  method: 'POST',\n  url: '/api/v1/items/',\n});\n"
     "return __request(OpenAPI, { url: '/api/v1/items/{id}', method: \"DELETE\" })"),
    ("java", "Res", "class", "src/Res.java",
     '@Path("/orders")\npublic class Res {\n  @GET\n  public List<O> all() {}\n  @POST public O add() {}\n}'),
    ("java", "Ctl", "class", "src/Ctl.java",
     '@RestController\n@RequestMapping("/owners")\nclass Ctl {\n  @GetMapping("/{id}")\n  O get() {}\n'
     '  @PostMapping\n  void add() {}\n  @DeleteMapping(value = "/x")\n  void d() {}\n}'),
    ("go", "routes", "function", "cmd/routes.go",
     'r.GET("/api/v1/ping", ping)\nr.POST(\'/users\', create)\ng.Group("/x")'),
    # API-prefix stripping and the look-ahead's backtracking.
    ("go", "prefixes", "function", "cmd/p.go",
     'r.GET("/api/v1x/a", h)\nr.GET("/api/v12/b", h)\nr.GET("/API/V2/c", h)\nr.GET("/api", h)\n'
     'r.GET("/apis/d", h)\nr.GET("/graphql/v1rc2", h)\nr.GET("/rest/v3alpha/e/", h)'),
    # ── gRPC servers ──
    ("proto", "svc", "service", "api/cart.proto",
     "service CartService {\n  rpc AddItem(AddItemRequest) returns (Empty) {}\n  rpc GetCart(GetCartRequest) returns (Cart);\n}\n"
     "service Other { rpc Ping(P) returns (P); }"),
    ("python", "CartServicer", "class", "srv/cart.py",
     "class CartServicer(object):\n    def AddItem(self, request, context): ...\n    def helper(self): ...\n    def GetCart( self, r, c): ..."),
    ("go", "srv", "interface", "pb/cart_grpc.pb.go",
     "type CartServiceServer interface {\n\tAddItem(context.Context, *Req) (*Empty, error)\n\tGetCart(ctx context.Context) error\n"
     "\tmustEmbedUnimplemented()\n}\nconst (\n\tCartService_AddItem_FullMethodName = \"/hipstershop.CartService/AddItem\"\n)"),
    ("java", "AdService", "class", "src/AdService.java",
     "class AdServiceImpl extends hipstershop.AdServiceGrpc.AdServiceImplBase {\n"
     "  public void getAds(AdRequest req, StreamObserver<AdResponse> obs) {}\n"
     "  public void listAds(Foo f, StreamObserver<X> o) {}\n  private void helper() {}\n}"),
    ("csharp", "CartService", "class", "src/CartService.cs",
     "public class CartService : Hipstershop.CartService.CartServiceBase {\n"
     "  public override async Task<Empty> AddItem(AddItemRequest r, ServerCallContext c) {}\n"
     "  public override Task<Cart> GetCart(GetCartRequest r, ServerCallContext c) {}\n}\n"
     "class Nope : A.B.CBase { public override Task X() {} }\n"
     "class Twice : X.Y.YBase.Z.ZBase { public override Task Q() {} }"),
    ("javascript", "main", "function", "src/server.js",
     "server.addService(proto.PaymentService.service, { charge: chargeHandler, refund });"),
    ("cpp", "Impl", "class", "src/impl.cc",
     "class Impl final : public Greeter::Service {\n  Status SayHello(ServerContext* ctx, const Req* r, Rep* p) override;\n"
     "  grpc::Status SayBye(grpc::ServerContext* c) {}\n};"),
    ("rust", "impl", "impl", "src/svc.rs",
     "#[tonic::async_trait]\nimpl Greeter for MyGreeter {\n    async fn say_hello(&self, r: Request<H>) {}\n"
     "    async fn _private(&self) {}\n}\nimpl fmt::Display for X { }\nimpl self_thing for Y { async fn z_y(&self) {} }"),
    # ── gRPC clients ──
    ("go", "call", "function", "fe/rpc.go",
     "resp, err := pb.NewCartServiceClient(fe.cartConn).\n\t\tGetCart(ctx, &pb.GetCartRequest{})\n"
     "cs := pb.NewCurrencyServiceClient(conn)\ncs.Convert(ctx, req)\ncs.helper()"),
    ("python", "recommend", "function", "rec/server.py",
     "stub = demo_pb2_grpc.ProductCatalogServiceStub(channel)\nresp = stub.ListProducts(Empty())\n"
     "x = ShippingServiceStub(ch).GetQuote(req)"),
    ("python", "use_stub", "function", "rec/other.py", "return catalog.ListProducts(req)"),
    ("java", "client", "class", "src/C.java",
     "private final AdServiceGrpc.AdServiceBlockingStub blockingStub;\n"
     "blockingStub = AdServiceGrpc.newBlockingStub(channel);\nblockingStub.getAds(req);"),
    ("csharp", "client", "method", "src/C.cs",
     "var client = new CartService.CartServiceClient(channel);\nawait client.GetCartAsync(req);\nclient.Async();"),
    ("typescript", "c", "function", "src/c.ts",
     "const c = new pkg.PaymentServiceClient(addr, creds); c.charge(req, cb);"),
    ("cpp", "c", "function", "src/c.cc",
     "std::unique_ptr<Greeter::Stub> stub_;\nauto s2 = helloworld::Greeter::NewStub(channel);\nstub_->SayHello(&ctx, req, &rep);"),
    ("rust", "c", "function", "src/c.rs",
     "let mut client = GreeterClient::connect(\"http://[::1]:50051\").await?;\nclient.say_hello(req).await?;"),
    # ── GraphQL, FFI ──
    ("graphql", "schema", "schema", "schema.graphql",
     "type Query {\n  me: User\n}\nextend type Mutation {\n  x: Int\n}\nEXTEND   TYPE subscription {}"),
    ("typescript", "R", "class", "src/r.ts", "@Resolver()\nclass R {\n  @Query(() => User)\n  me() {}\n  @FieldResolver() f() {}\n}"),
    ("rust", "compute_hash", "function", "src/ffi.rs",
     '#[no_mangle]\npub extern "C" fn compute_hash(p: *const u8) -> u64 { 0 }'),
    ("java", "Native", "class", "src/N.java", "class N { public native int add(int a, int b); }"),
    ("csharp", "compute_hash", "method", "src/N.cs",
     '[DllImport("libnative")]\nstatic extern ulong compute_hash(byte[] d);\n[DllImport(\'compute_hash\')] static extern void x();'),
    ("rust", "greet", "function", "src/w.rs", "#[wasm_bindgen]\npub fn greet() {}"),
    ("json", "", "json_document", "a/.oxlintrc.json", '[File: a/.oxlintrc.json]\n{"rules": {"extern \'C\'": 1}}'),
    # ── Data shapes ──
    ("python", "UserPublic", "class", "models.py",
     "class UserPublic(BaseModel):\n    id: int\n    full_name: str | None = None  # c\n    __slots__: tuple\n"
     "    def f(self) -> int: ...\nclass Empty:\n    pass\nclass Item(SQLModel, table=True):\n    ownerId: uuid.UUID = Field()"),
    ("typescript", "UserPublic", "type_alias", "client/types.gen.ts",
     "export type UserPublic = {\n  id: string;\n  readonly full_name?: string | null;\n  isActive: boolean,\n};\n"
     "export interface Item extends Base<T> { title: string; ownerId: string }\ninterface E {}"),
    ("go", "User", "struct", "models.go",
     "type User struct {\n\tID   string `json:\"id\"`\n\tFullName string `json:\"full_name,omitempty\"`\n\tage int\n\tTags []string\n}"),
    ("java", "Owner", "class", "Owner.java",
     "public class Owner extends Person {\n  private String address;\n  protected final List<Pet> pets = new ArrayList<>();\n"
     "  public String getAddress() { return address; }\n}\npublic record Point(int x, Map<String, List<Integer>> y, final long z) {}"),
    ("rust", "Order", "struct", "src/order.rs",
     "pub struct Order {\n    #[serde(rename = \"orderId\")]\n    pub id: u64,\n    pub total_cents: i64,\n    items: Vec<Item>,\n}"),
    ("csharp", "Order", "class", "Order.cs",
     "public sealed class Order : Entity {\n  public int OrderId { get; set; }\n  internal string Name = \"\";\n"
     "  private readonly List<Item> _items;\n  public void Do() {}\n}\npublic record Money(decimal Amount, string Currency);"),
    # ── BDD, CLI ──
    ("python", "step", "function", "steps.py", "@given('a user named \"x\"')\n@then(\"it works\")\ndef step(): ..."),
    ("gherkin", "f", "document", "features/login.feature",
     "Feature: login\n  Scenario: ok\n    Given a registered user\n    When  they log in  \n    And But nothing\n    Then it works"),
    ("markdown", "Doc", "markdown_section", "docs/x.md", "Given a markdown file\nThen no step"),
    ("python", "cli", "function", "cli.py",
     "@click.command('serve')\n@cli.group(name=\"db\")\nsub = p.add_subparsers(dest='c').add_parser('init')"),
    ("go", "cmd", "function", "cmd/root.go", 'var rootCmd = &cobra.Command{\n\tUse: "serve [flags]",\n}'),
    ("go", "cmd", "function", "cmd/blank.go", 'var c = &cobra.Command{ Use: "   ", }\nr.GET("/x", h)'),
    # ── Pylon class APIs ──
    ("python", "API", "class", "plugins/configurations/api/v1/configurations.py",
     "class API(api_tools.APIBase):\n    url_params = [\n        '<int:project_id>',\n        '',\n        \"<string:mode>/<int:project_id>\",\n    ]\n"
     "    def get(self, project_id): ...\n    def POST(self): ...\n    def helper(self): ..."),
    ("python", "API", "class", "api/v2/foo.py", "class API(MethodView):\n    def delete(self): ..."),
    ("python", "API", "function", "api/v2/foo.py", "class API(MethodView):\n    def delete(self): ..."),
    # Unicode word characters (Python \w: letters and numerics, no marks).
    ("go", "u", "function", "u.go", 'r.GET("/ünï²", h)\ntype Ünï struct {\n\tÄge int `json:"âge"`\n}'),
]

PLUGIN_NAMES = {"plugins/configurations/api/v1/configurations.py": "configurations", "api/v2/foo.py": "configurations"}
BINDINGS = {"rec/other.py": {"catalog": "ProductCatalogService"}}


def surfaces_of(case):
    language, symbol_name, symbol_type, rel_path, text = case
    node = {
        "language": language,
        "symbol_name": symbol_name,
        "symbol_type": symbol_type,
        "rel_path": rel_path,
        "source_text": text,
    }
    try:
        found = ase.extract_api_surfaces(
            node,
            plugin_name=PLUGIN_NAMES.get(rel_path, ""),
            grpc_stub_bindings=BINDINGS.get(rel_path),
        )
    except Exception:  # noqa: BLE001 - the port mirrors the raise as None
        return None
    return [
        {"kind": s["kind"], "surface": s["surface"], "weight_hint": s["weight_hint"], "metadata": s["metadata"]}
        for s in found
    ]


def main() -> int:
    out = {
        "cases": [
            {
                "language": c[0],
                "symbol_name": c[1],
                "symbol_type": c[2],
                "rel_path": c[3],
                "source_text": c[4],
                "plugin_name": PLUGIN_NAMES.get(c[3], ""),
                "bindings": BINDINGS.get(c[3]),
                "expected": surfaces_of(c),
            }
            for c in CASES
        ],
        "to_snake": {n: ase._to_snake(n) for n in ["OrderId", "computeHash", "HTTPRequest", "order_id", "ID", "aB1C", "Ünï"]},
        "strip_prefix": {
            p: ase._strip_common_api_prefix(p)
            for p in ["/api/v1/items", "/api", "/apis/x", "/api/v1x/a", "/API/V2/c/", "/graphql/v1rc2", "rest/v3alpha/e", "/x"]
        },
        "pylon_endpoints": ase.derive_pylon_endpoints(
            "plugins/configurations/api/v1/configurations.py",
            CASES[-4][4],
            "configurations",
        ),
        "plugin_names": {
            t: ase.plugin_name_from_metadata_text(t)
            for t in ['[File: metadata.json]\n{"name": "configurations"}', '{"name": " x-y_1 "}', '{"name": "1bad"}', '{"name": 5}', "[1]", "nope"]
        },
    }
    with open(sys.argv[1], "w", encoding="utf-8") as handle:
        json.dump(out, handle, indent=1, ensure_ascii=False)
        handle.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
