// Forwards every request to one shop container instance.
import { Container, getContainer } from "@cloudflare/containers";

export class ShopContainer extends Container {
  defaultPort = 8080;
  sleepAfter = "10m";
}

export default {
  async fetch(request: Request, env: { SHOP: DurableObjectNamespace<ShopContainer> }): Promise<Response> {
    return getContainer(env.SHOP).fetch(request);
  },
};
