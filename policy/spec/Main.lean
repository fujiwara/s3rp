import Lean.Data.Json
import PolicySpec.Semantics

/-!
The specification as a program, for comparing an implementation with it
(internal/policyspec). Each input line is a JSON object — a bucket policy, a
user policy and requests — and each output line the decisions for those
requests, in order.
-/

open Lean PolicySpec

structure WireEntry where
  tenant : String
  /-- absent for `tenant/*` -/
  user : Option String
  deriving FromJson

structure WireRange where
  /-- the address in hex: 8 digits for IPv4, 32 for IPv6 -/
  addr : String
  bits : Nat
  deriving FromJson

structure WireStatement where
  effect : String
  /-- `everyone`, `listed` or `allBut` -/
  subject : String
  entries : List WireEntry
  actions : List String
  resources : List String
  ipAddress : List WireRange
  notIpAddress : List WireRange
  deriving FromJson

structure WireUserStatement where
  effect : String
  actions : List String
  deriving FromJson

structure WireRequest where
  tenant : String
  user : String
  action : String
  resource : String
  /-- absent when the source address is unknown -/
  source : Option String
  deriving FromJson

structure WireCase where
  /-- the tenant owning the bucket; absent when only the policies are asked -/
  owner : Option String
  statements : List WireStatement
  userPolicy : List WireUserStatement
  requests : List WireRequest
  deriving FromJson

def hexDigit (c : Char) : Except String Nat :=
  if '0' ≤ c ∧ c ≤ '9' then pure (c.toNat - '0'.toNat)
  else if 'a' ≤ c ∧ c ≤ 'f' then pure (c.toNat - 'a'.toNat + 10)
  else throw s!"not a hex digit: {c}"

def addrOfHex (hex : String) : Except String Addr := do
  let digits ← hex.toList.mapM hexDigit
  let bits := digits.flatMap fun d => [8, 4, 2, 1].map fun w => d / w % 2 == 1
  if bits.length == 32 || bits.length == 128 then pure bits
  else throw s!"an address of {bits.length} bits"

def effectOf : String → Except String Effect
  | "Allow" => pure .allow
  | "Deny" => pure .deny
  | other => throw s!"effect {other}"

def rangeOf (r : WireRange) : Except String Range := do
  pure ⟨← addrOfHex r.addr, r.bits⟩

def entryOf (e : WireEntry) : Entry :=
  match e.user with
  | some u => .user e.tenant u
  | none => .tenant e.tenant

def statementOf (s : WireStatement) : Except String Statement := do
  let entries := s.entries.map entryOf
  let subject ← match s.subject with
    | "everyone" => pure Subject.everyone
    | "listed" => pure (Subject.listed entries)
    | "allBut" => pure (Subject.allBut entries)
    | other => throw s!"subject {other}"
  pure {
    effect := ← effectOf s.effect
    subject
    actions := s.actions
    resources := s.resources
    condition := ⟨← s.ipAddress.mapM rangeOf, ← s.notIpAddress.mapM rangeOf⟩
  }

def requestOf (r : WireRequest) : Except String Request := do
  let source ← match r.source with
    | some hex => some <$> addrOfHex hex
    | none => pure none
  pure ⟨⟨r.tenant, r.user⟩, r.action, r.resource, source⟩

def answer (line : String) : Except String Json := do
  let c : WireCase ← fromJson? (← Json.parse line)
  let policy : Policy := ⟨c.owner.getD "", ← c.statements.mapM statementOf⟩
  let userPolicy ← c.userPolicy.mapM fun s => do
    pure (⟨← effectOf s.effect, s.actions⟩ : UserStatement)
  let results ← c.requests.mapM fun w => do
    let r ← requestOf w
    let (effect, statement) : String × Int := match policy.decide r with
      | .deny i => ("deny", i)
      | .allow i => ("allow", i)
      | .none => ("none", -1)
    pure <| Json.mkObj [
      ("effect", effect),
      ("statement", toJson statement),
      ("mentions", policy.mentions r.principal),
      ("userAllows", userAllows userPolicy r.action),
      ("visible", policy.visible r.principal),
      ("permits", permits userPolicy policy r)]
  pure (Json.arr results.toArray)

partial def serve (stdin stdout : IO.FS.Stream) : IO UInt32 := do
  let line ← stdin.getLine
  if line.isEmpty then
    return 0
  match answer line with
  | .ok json =>
    stdout.putStrLn json.compress
    -- the caller waits for each answer before it sends the next case
    stdout.flush
    serve stdin stdout
  | .error e =>
    IO.eprintln s!"policyspec: {e}"
    return 1

def main : IO UInt32 := do
  serve (← IO.getStdin) (← IO.getStdout)
