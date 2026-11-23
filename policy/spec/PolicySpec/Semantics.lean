/-!
The policy semantics: what a bucket policy and a user policy decide, and what
the gateway makes of the two.
-/

namespace PolicySpec

/-! ## Patterns -/

/-- A pattern against a value: `*` is any run of characters, `?` exactly
one, anything else itself. There is no escaping. -/
def glob : List Char → List Char → Bool
  | [], v => v.isEmpty
  | '*' :: p, [] => glob p []
  | '*' :: p, c :: v => glob p (c :: v) || glob ('*' :: p) v
  | _ :: _, [] => false
  | c :: p, d :: v => (c == '?' || c == d) && glob p v
termination_by p v => p.length + v.length

/-- A resource pattern against a resource, case-sensitive. -/
def matchResource (pattern value : String) : Bool :=
  glob pattern.toList value.toList

/-- An action pattern against an action. `A` to `Z` compare as `a` to `z`;
actions are ASCII. -/
def matchAction (pattern value : String) : Bool :=
  glob (pattern.toList.map Char.toLower) (value.toList.map Char.toLower)

/-! ## Addresses -/

/-- An address as its bits: 32 for IPv4, 128 for IPv6. -/
abbrev Addr := List Bool

/-- A condition value: the addresses sharing the first `bits` bits of `addr`. -/
structure Range where
  addr : Addr
  bits : Nat
  deriving Repr

/-- The 96 bits before the IPv4 address in an IPv4-mapped IPv6 address,
`::ffff:0:0/96`. -/
def mappedPrefix : List Bool :=
  List.replicate 80 false ++ List.replicate 16 true

def Addr.isMapped (a : Addr) : Bool :=
  a.length == 128 && a.take 96 == mappedPrefix

/-- An IPv4-mapped IPv6 address is the IPv4 address it maps. -/
def Addr.unmap (a : Addr) : Addr :=
  if a.isMapped then a.drop 96 else a

/-- An IPv4-mapped range is the IPv4 range it maps. One wider than the
mapped addresses is no range; `Parse` refuses it. -/
def Range.unmap (r : Range) : Option Range :=
  if r.addr.isMapped then
    if r.bits < 96 then none else some ⟨r.addr.drop 96, r.bits - 96⟩
  else
    some r

/-- Whether a range includes a source address. The families never meet. -/
def contains (value : Range) (source : Addr) : Bool :=
  match value.unmap with
  | none => false
  | some r =>
    let s := source.unmap
    r.addr.length == s.length && r.addr.take r.bits == s.take r.bits

/-! ## Statements -/

inductive Effect where
  | allow
  | deny
  deriving DecidableEq, Repr

/-- The requester, `tenant/user`. -/
structure Principal where
  tenant : String
  user : String
  deriving DecidableEq, Repr

/-- One name in a statement's principal list. -/
inductive Entry where
  /-- `tenant/user` -/
  | user (tenant user : String)
  /-- `tenant/*` -/
  | tenant (tenant : String)
  deriving Repr

def Entry.covers : Entry → Principal → Bool
  | .user t u, p => t == p.tenant && u == p.user
  | .tenant t, p => t == p.tenant

/-- Whom a statement is about. -/
inductive Subject where
  /-- `"Principal": "*"` -/
  | everyone
  /-- `"Principal": {"S3RP": [...]}` -/
  | listed (entries : List Entry)
  /-- `"NotPrincipal": {"S3RP": [...]}` -/
  | allBut (entries : List Entry)
  deriving Repr

def Subject.includes : Subject → Principal → Bool
  | .everyone, _ => true
  | .listed es, p => es.any (·.covers p)
  | .allBut es, p => !es.any (·.covers p)

structure Condition where
  ipAddress : List Range := []
  notIpAddress : List Range := []
  deriving Repr

/-- Every operator present must hold. `source` is `none` when the address of
the request is unknown. -/
def Condition.holds (c : Condition) (source : Option Addr) : Bool :=
  (c.ipAddress.isEmpty ||
    match source with
    | some a => c.ipAddress.any (contains · a)
    | none => false) &&
  (c.notIpAddress.isEmpty ||
    match source with
    | some a => !c.notIpAddress.any (contains · a)
    | none => true)

structure Request where
  principal : Principal
  action : String
  resource : String
  source : Option Addr

structure Statement where
  effect : Effect
  subject : Subject
  actions : List String
  resources : List String
  condition : Condition := {}
  deriving Repr

def Statement.applies (s : Statement) (r : Request) : Bool :=
  s.subject.includes r.principal &&
  s.condition.holds r.source &&
  s.actions.any (matchAction · r.action) &&
  s.resources.any (matchResource · r.resource)

/-- A bucket policy, with the tenant that owns the bucket. -/
structure Policy where
  owner : String
  statements : List Statement

/-- What `Parse` guarantees and the properties rely on: `NotPrincipal` comes
with `Deny` only. -/
def Policy.WellFormed (p : Policy) : Prop :=
  ∀ s ∈ p.statements, s.effect = .allow → ∀ es, s.subject ≠ .allBut es

/-- The decision of a policy, with the index of the statement that made it. -/
inductive Decision where
  | deny (statement : Nat)
  | allow (statement : Nat)
  | none
  deriving DecidableEq, Repr

/-- The first statement of the effect that applies. -/
def Policy.first (p : Policy) (e : Effect) (r : Request) : Option Nat :=
  p.statements.findIdx? fun s => s.effect == e && s.applies r

/-- An applying Deny decides, else an applying Allow, else nothing does. -/
def Policy.decide (p : Policy) (r : Request) : Decision :=
  match p.first .deny r, p.first .allow r with
  | some i, _ => .deny i
  | Option.none, some i => .allow i
  | Option.none, Option.none => .none

/-- Some Allow statement names the principal, whatever its action, resource
and condition. -/
def Policy.mentions (p : Policy) (who : Principal) : Bool :=
  p.statements.any fun s =>
    s.effect == .allow &&
    match s.subject with
    | .everyone => true
    | .listed es => es.any (·.covers who)
    | .allBut _ => false

/-- A statement of a user policy: an effect over actions. -/
structure UserStatement where
  effect : Effect
  actions : List String

def UserStatement.applies (s : UserStatement) (action : String) : Bool :=
  s.actions.any (matchAction · action)

/-- A user policy. The empty one allows every action; otherwise an action
needs an Allow and no Deny. -/
def userAllows (up : List UserStatement) (action : String) : Bool :=
  up.isEmpty ||
  (!up.any (fun s => s.effect == .deny && s.applies action) &&
    up.any (fun s => s.effect == .allow && s.applies action))

/-- Whether the bucket exists as far as the requester can tell. -/
def Policy.visible (p : Policy) (who : Principal) : Bool :=
  who.tenant == p.owner || p.mentions who

/-- What the gateway decides: the user policy must allow the action, the
bucket must be visible, and the bucket policy must not deny a user of the
owning tenant, or must allow anyone else. -/
def permits (up : List UserStatement) (p : Policy) (r : Request) : Bool :=
  userAllows up r.action &&
  p.visible r.principal &&
  match p.decide r with
  | .deny _ => false
  | .allow _ => true
  | .none => r.principal.tenant == p.owner

end PolicySpec
