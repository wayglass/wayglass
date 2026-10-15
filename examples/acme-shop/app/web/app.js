// Renders the product grid from the JSON API.
const symbols = { EUR: "€", USD: "$", GBP: "£" };

function formatPrice(cents, currency) {
  const amount = (cents / 100).toFixed(2);
  return symbols[currency] ? symbols[currency] + amount : `${amount} ${currency}`;
}

async function load() {
  const list = document.getElementById("products");
  const res = await fetch("/api/products");
  const { products } = await res.json();
  list.replaceChildren(
    ...products.map((p) => {
      const li = document.createElement("li");
      li.className = p.in_stock ? "card" : "card sold-out";
      li.innerHTML = `<h2></h2><p class="cat"></p><p class="price"></p>`;
      li.querySelector("h2").textContent = p.name;
      li.querySelector(".cat").textContent = p.category;
      li.querySelector(".price").textContent = p.in_stock ? formatPrice(p.price_cents, p.currency) : "Sold out";
      return li;
    }),
  );
}

load();
