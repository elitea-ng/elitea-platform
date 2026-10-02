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
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";

import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import { server } from "@/test/setup";

import { AdminScimClientsEditor } from "./AdminScimClientsEditor";
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
      recorded.push({ method: "POST", url: request.url, body: null });
      return HttpResponse.json({ client: BEARER_CLIENT, secret: BEARER_SECRET });
    }),
    http.post(`${BASE}/:id/revoke`, ({ request }) => {
      recorded.push({ method: "POST", url: request.url, body: null });
      return HttpResponse.json({
        client: { ...BEARER_CLIENT, status: "revoked" },
      });
    }),
    http.delete(`${BASE}/:id`, ({ request }) => {
      recorded.push({ method: "DELETE", url: request.url, body: null });
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

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.body).toEqual({ name: "Okta", auth_method: "bearer" });

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

    await screen.findByTestId("admin-scim-client-secret-dialog");
    expect(writes()[0]?.body).toEqual({
      name: "Entra CC",
      auth_method: "client_credentials",
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

    expect(
      await screen.findByTestId("admin-scim-client-create-error"),
    ).toHaveTextContent("already exists");
    expect(screen.getByTestId("admin-scim-client-name")).toHaveValue("Okta");
    expect(
      screen.queryByTestId("admin-scim-client-secret-dialog"),
    ).not.toBeInTheDocument();
  });

  it("confirms a rotate, then reveals the new secret", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-client-rotate-c1"));
    const confirm = await screen.findByTestId(
      "admin-scim-client-confirm-dialog",
    );
    expect(confirm).toHaveTextContent("stops working immediately");
    expect(writes()).toHaveLength(0);

    await user.click(screen.getByTestId("admin-scim-client-confirm"));
    expect(
      await screen.findByTestId("admin-scim-client-reveal-secret"),
    ).toHaveValue(BEARER_SECRET);
    expect(writes()[0]?.url).toContain(
      "/admin/scim_clients/administration/c1/rotate",
    );
  });

  it("confirms a revoke and posts to the revoke endpoint", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-client-revoke-c2"));
    expect(
      await screen.findByTestId("admin-scim-client-confirm-dialog"),
    ).toHaveTextContent("can no longer provision");
    await user.click(screen.getByTestId("admin-scim-client-confirm"));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.method).toBe("POST");
    expect(writes()[0]?.url).toContain(
      "/admin/scim_clients/administration/c2/revoke",
    );
  });

  it("confirms a delete and deletes by id", async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminScimClientsEditor />);
    await screen.findByText("Entra production");

    await user.click(screen.getByTestId("admin-scim-client-delete-c1"));
    await user.click(await screen.findByTestId("admin-scim-client-confirm"));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.method).toBe("DELETE");
    expect(writes()[0]?.url).toContain("/admin/scim_clients/administration/c1");
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
});
