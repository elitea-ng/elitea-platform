/**
 * Rendering + write-path guard for the SCIM client credential panel.
 *
 *  1. **The connection facts an identity provider needs are on screen**: the
 *     Tenant URL and the token endpoint, built from this origin.
 *  2. **A listed client never shows a secret** — only its last four
 *     characters.
 *  3. **A create or a rotate reveals the secret ONCE**, and closing the reveal
 *     drops it: it is not in the DOM afterwards.
 *  4. **Every breaking action is confirmed first**, and the confirmation hits
 *     the right endpoint.
 *
 * No fixture value here is or resembles a real credential.
 */
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";

import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import { server } from "@/test/setup";

import { daysUntil, expiryInstant } from "./AdminScimClientExpiry";
import { AdminScimClientsEditor } from "./AdminScimClientsEditor";
import { confirmDangerousAction } from "./__tests__/dangerousDialog";
import { renderAdminRoute } from "./__tests__/testRouter";

const BEARER_CLIENT = {
  id: "c1",
  name: "Entra production",
  auth_method: "bearer" as const,
  secret_hint: "ab12",
  status: "active" as const,
  created_by: 1,
  created_by_name: "admin",
  created_at: "2026-09-01T10:00:00Z",
  last_used_at: null,
};

const CC_CLIENT = {
  id: "c2",
  name: "Entra staging",
  auth_method: "client_credentials" as const,
  client_id: "scimc_fixture-client",
  secret_hint: "cd34",
  status: "active" as const,
  created_at: "2026-09-02T10:00:00Z",
  last_used_at: "2026-09-03T10:00:00Z",
  // Inside the 30-day warning window, whatever day the suite runs on.
  expires_at: new Date(Date.now() + 10 * 24 * 60 * 60 * 1000).toISOString(),
};

const BEARER_SECRET = "scim_fixture-not-a-real-secret-ab12";
const CC_SECRET = "scimcs_fixture-not-a-real-secret-cd34";

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

const BASE = "*/admin/scim_clients/administration";

/** The reason of a bodiless dangerous action travels in its header (ADR-0059 R3.8b). */
function reasonCode(request: Request): { reasonCode: string | null } {
  return { reasonCode: request.headers.get("Elitea-Reason-Code") };
}

function useClientHandlers(
  options: { createStatus?: number; createBody?: Record<string, string> } = {},
): void {
  server.use(
    http.get(BASE, ({ request }) => {
      recorded.push({ method: "GET", url: request.url, body: null });
      return HttpResponse.json({
        clients: [BEARER_CLIENT, CC_CLIENT],
        total: 2,
        scim_base_path: "/api/v2/scim/v2",
        token_endpoint_path: "/api/v2/scim/oauth/token",
        access_token_ttl_seconds: 3600,
      });
    }),
    http.post(BASE, async ({ request }) => {
      const body = (await request.json()) as {
        name: string;
        auth_method: string;
      };
      recorded.push({ method: "POST", url: request.url, body });
      if (options.createStatus !== undefined) {
        return HttpResponse.json(options.createBody, {
          status: options.createStatus,
        });
      }
      const isCC = body.auth_method === "client_credentials";
      const client = {
        ...(isCC ? CC_CLIENT : BEARER_CLIENT),
        id: "new",
        name: body.name,
      };
      return HttpResponse.json(
        {
          client,
          secret: isCC ? CC_SECRET : BEARER_SECRET,
          ...(isCC ? { client_id: CC_CLIENT.client_id } : {}),
          scim_base_path: "/api/v2/scim/v2",
          token_endpoint_path: "/api/v2/scim/oauth/token",
        },
        { status: 201 },
      );
    }),
    http.post(`${BASE}/:id/rotate`, ({ request }) => {
      recorded.push({ method: "POST", url: request.url, body: reasonCode(request) });
      return HttpResponse.json({ client: BEARER_CLIENT, secret: BEARER_SECRET });
    }),
    http.post(`${BASE}/:id/revoke`, ({ request }) => {
      recorded.push({ method: "POST", url: request.url, body: reasonCode(request) });
      return HttpResponse.json({
        client: { ...BEARER_CLIENT, status: "revoked" },
      });
    }),
    http.delete(`${BASE}/:id`, ({ request }) => {
      recorded.push({ method: "DELETE", url: request.url, body: reasonCode(request) });
      return new HttpResponse(null, { status: 204 });
    }),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== "GET");
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: "/api/v2" });
  useClientHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

describe("Admin › Authentication › SCIM clients", () => {
  it("shows the Tenant URL and the token endpoint for this origin", async () => {
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    const origin = window.location.origin;
    expect(screen.getByTestId("admin-scim-clients-tenant-url")).toHaveValue(
      `${origin}/api/v2/scim/v2`,
    );
    expect(
      screen.getByTestId("admin-scim-clients-token-endpoint"),
    ).toHaveValue(`${origin}/api/v2/scim/oauth/token`);
    expect(
      screen.getByRole("button", { name: "Copy Tenant URL" }),
    ).toBeInTheDocument();
  });

  it("copies the Tenant URL and says so", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-tenant-url-copy"));
    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe(
        `${window.location.origin}/api/v2/scim/v2`,
      );
    });
    expect(
      await screen.findByRole("button", { name: "Copied" }),
    ).toBeInTheDocument();
  });

  it("lists clients by method with only the secret ending", async () => {
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    const bearerRow = screen.getByTestId("admin-scim-client-row-c1");
    expect(bearerRow).toHaveTextContent("Secret token (Bearer)");
    expect(bearerRow).toHaveTextContent("…ab12");
    expect(bearerRow).toHaveTextContent("Never");
    const ccRow = screen.getByTestId("admin-scim-client-row-c2");
    expect(ccRow).toHaveTextContent("OAuth2 client credentials");
    expect(ccRow).toHaveTextContent("scimc_fixture-client");
    expect(document.body).not.toHaveTextContent(BEARER_SECRET);
    expect(document.body).not.toHaveTextContent(CC_SECRET);
  });

  it("creates a bearer client and reveals its secret exactly once", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Okta");
    await user.click(screen.getByTestId("admin-scim-client-create-submit"));
    await confirmDangerousAction(user, "Okta");

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.body).toEqual({
      name: "Okta",
      auth_method: "bearer",
      reason_code: "incident",
      reason: "ticket 42",
    });

    const reveal = await screen.findByTestId("admin-scim-client-secret-dialog");
    expect(reveal).toHaveTextContent("It is shown only once");
    expect(screen.getByTestId("admin-scim-client-reveal-secret")).toHaveValue(
      BEARER_SECRET,
    );
    expect(
      screen.queryByTestId("admin-scim-client-reveal-client-id"),
    ).not.toBeInTheDocument();

    await user.click(screen.getByTestId("admin-scim-client-reveal-close"));
    await waitFor(() => {
      expect(
        screen.queryByTestId("admin-scim-client-secret-dialog"),
      ).not.toBeInTheDocument();
    });
    expect(
      screen.queryByDisplayValue(BEARER_SECRET),
    ).not.toBeInTheDocument();
  });

  it("keeps the one-time secret open on Escape; only the close button closes it", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Okta");
    await user.click(screen.getByTestId("admin-scim-client-create-submit"));
    await confirmDangerousAction(user, "Okta");
    await screen.findByTestId("admin-scim-client-secret-dialog");

    await user.keyboard("{Escape}");
    expect(
      screen.getByTestId("admin-scim-client-secret-dialog"),
    ).toBeInTheDocument();
    expect(screen.getByTestId("admin-scim-client-reveal-secret")).toHaveValue(
      BEARER_SECRET,
    );

    await user.click(screen.getByTestId("admin-scim-client-reveal-close"));
    await waitFor(() => {
      expect(
        screen.queryByTestId("admin-scim-client-secret-dialog"),
      ).not.toBeInTheDocument();
    });
  });

  it("creates a client-credentials client and reveals id, secret and token endpoint", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Entra CC");
    await user.click(
      within(
        screen.getByTestId("admin-scim-client-method-client_credentials"),
      ).getByRole("radio"),
    );
    await user.click(screen.getByTestId("admin-scim-client-create-submit"));
    await confirmDangerousAction(user, "Entra CC");

    await screen.findByTestId("admin-scim-client-secret-dialog");
    expect(writes()[0]?.body).toEqual({
      name: "Entra CC",
      auth_method: "client_credentials",
      reason_code: "incident",
      reason: "ticket 42",
    });
    expect(
      screen.getByTestId("admin-scim-client-reveal-client-id"),
    ).toHaveValue("scimc_fixture-client");
    expect(screen.getByTestId("admin-scim-client-reveal-secret")).toHaveValue(
      CC_SECRET,
    );
    expect(
      screen.getByTestId("admin-scim-client-reveal-token-endpoint"),
    ).toHaveValue(`${window.location.origin}/api/v2/scim/oauth/token`);
  });

  it("keeps the create dialog open on a refusal with the server sentence", async () => {
    useClientHandlers({
      createStatus: 409,
      createBody: { error: "a SCIM client named Okta already exists" },
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Okta");
    await user.click(screen.getByTestId("admin-scim-client-create-submit"));
    await confirmDangerousAction(user, "Okta");

    expect(
      await screen.findByTestId("admin-scim-client-create-error"),
    ).toHaveTextContent("already exists");
    expect(screen.getByTestId("admin-scim-client-name")).toHaveValue("Okta");
    expect(
      screen.queryByTestId("admin-scim-client-secret-dialog"),
    ).not.toBeInTheDocument();
  });

  it("explains that only a super admin can create a SCIM client (#2558)", async () => {
    useClientHandlers({
      createStatus: 403,
      createBody: {
        error: "only a super admin can create, rotate or delete a SCIM client",
        code: "scim_client_super_admin_required",
      },
    });
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Okta");
    await user.click(screen.getByTestId("admin-scim-client-create-submit"));
    await confirmDangerousAction(user, "Okta");

    expect(
      await screen.findByTestId("admin-scim-client-create-error"),
    ).toHaveTextContent(
      "Only a super admin can create, rotate or delete a SCIM client.",
    );
    expect(
      screen.queryByTestId("admin-scim-client-secret-dialog"),
    ).not.toBeInTheDocument();
  });

  it("confirms a rotate, then reveals the new secret", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-client-rotate-c1"));
    const confirm = await screen.findByTestId("admin-dangerous-dialog");
    expect(confirm).toHaveTextContent("stops working immediately");
    expect(confirm).toHaveTextContent("Critical action");
    expect(writes()).toHaveLength(0);

    await confirmDangerousAction(user, "Entra production");
    expect(
      await screen.findByTestId("admin-scim-client-reveal-secret"),
    ).toHaveValue(BEARER_SECRET);
    expect(writes()[0]?.url).toContain(
      "/admin/scim_clients/administration/c1/rotate",
    );
    expect(writes()[0]?.body).toEqual({ reasonCode: "incident" });
  });

  it("confirms a revoke and posts to the revoke endpoint", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-client-revoke-c2"));
    expect(
      await screen.findByTestId("admin-dangerous-dialog"),
    ).toHaveTextContent("can no longer provision");
    await confirmDangerousAction(user, "Entra staging", { code: "offboarding" });

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.method).toBe("POST");
    expect(writes()[0]?.url).toContain(
      "/admin/scim_clients/administration/c2/revoke",
    );
    expect(writes()[0]?.body).toEqual({ reasonCode: "offboarding" });
  });

  it("confirms a delete and deletes by id", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-client-delete-c1"));
    await confirmDangerousAction(user, "Entra production");

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.method).toBe("DELETE");
    expect(writes()[0]?.url).toContain("/admin/scim_clients/administration/c1");
    expect(writes()[0]?.body).toEqual({ reasonCode: "incident" });
  });

  it("names the missing permission on a 403", async () => {
    server.use(
      http.get(BASE, () =>
        HttpResponse.json({ error: "forbidden" }, { status: 403 }),
      ),
    );
    renderAdminRoute(<AdminScimClientsEditor />);

    expect(
      await screen.findByTestId("admin-scim-clients-error"),
    ).toHaveTextContent("admin.auth.users");
    expect(
      screen.queryByTestId("admin-scim-clients-empty"),
    ).not.toBeInTheDocument();
  });

  it("shows the days left and warns inside 30 days", async () => {
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    expect(screen.getByTestId("admin-scim-client-row-c1")).toHaveTextContent(
      "No expiry",
    );
    const ccRow = screen.getByTestId("admin-scim-client-row-c2");
    expect(
      within(ccRow).getByTestId("admin-scim-client-expiry-warning"),
    ).toHaveTextContent("Expires in 10 days");
  });

  it("sends an optional expiry as the end of the chosen day", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    const inYear = new Date();
    inYear.setDate(inYear.getDate() + 365);
    const value = `${inYear.getFullYear()}-${String(inYear.getMonth() + 1).padStart(2, "0")}-${String(inYear.getDate()).padStart(2, "0")}`;

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Okta");
    const dateInput = screen.getByTestId("admin-scim-client-expires-on");
    await user.clear(dateInput);
    await user.type(dateInput, value);
    await user.click(screen.getByTestId("admin-scim-client-create-submit"));
    await confirmDangerousAction(user, "Okta");

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.body).toEqual({
      name: "Okta",
      auth_method: "bearer",
      expires_at: expiryInstant(value),
      reason_code: "incident",
      reason: "ticket 42",
    });
  });

  it("refuses an expiry in the past", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-clients-add"));
    await user.type(screen.getByTestId("admin-scim-client-name"), "Okta");
    const dateInput = screen.getByTestId("admin-scim-client-expires-on");
    await user.clear(dateInput);
    await user.type(dateInput, "2020-01-01");
    expect(screen.getByTestId("admin-scim-client-create-submit")).toBeDisabled();
    expect(daysUntil("2020-01-01T00:00:00Z")).toBeLessThan(0);
  });
});

/**
 * The trust split (ADR-0059 decision 6, R3.6): creating, rotating and
 * deleting a SCIM client are trust actions, open only through the
 * identity-trust item. The documents are the server's own goldens, for a
 * holder of identity.sso.settings.manage alone and for admin.
 */
describe("the SCIM client trust actions", () => {
  const GOLDEN_DIR = join(
    process.cwd(),
    "../../services/elitea-main/internal/api/v2/adminaccess/testdata/console",
  );

  for (const holder of ["identity_settings", "admin"]) {
    it(`are read-only for ${holder}, with the notice, while revoke stays open`, async () => {
      const document = JSON.parse(
        readFileSync(join(GOLDEN_DIR, `${holder}.golden.json`), "utf8"),
      ) as Record<string, unknown>;
      server.use(http.get("*/admin/console", () => HttpResponse.json(document)));
      renderAdminRoute(<AdminScimClientsEditor />);

      expect(await screen.findByTestId("admin-identity-trust-notice")).toHaveTextContent(
        /global administrator/i,
      );
      await waitFor(() => expect(screen.getByTestId("admin-scim-clients-add")).toBeDisabled());
      expect(await screen.findByTestId("admin-scim-client-rotate-c1")).toBeDisabled();
      expect(screen.getByTestId("admin-scim-client-delete-c1")).toBeDisabled();
      expect(screen.getByTestId("admin-scim-client-revoke-c1")).toBeEnabled();
      expect(writes()).toEqual([]);
    });
  }

  it("are open to the global administrator", async () => {
    const document = JSON.parse(
      readFileSync(join(GOLDEN_DIR, "super_admin.golden.json"), "utf8"),
    ) as Record<string, unknown>;
    server.use(http.get("*/admin/console", () => HttpResponse.json(document)));
    renderAdminRoute(<AdminScimClientsEditor />);

    expect(await screen.findByTestId("admin-scim-client-rotate-c1")).toBeEnabled();
    expect(screen.getByTestId("admin-scim-clients-add")).toBeEnabled();
    expect(screen.queryByTestId("admin-identity-trust-notice")).toBeNull();
  });
});
